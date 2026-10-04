// Command assetpack 是内容寻址的数字资产资源打包工具(非通用压缩工具)。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"assetpack"
)

type SpecEntry struct {
	Path      string   `json:"path"`
	File      string   `json:"file,omitempty"`
	Inline    string   `json:"inline,omitempty"`
	Dir       bool     `json:"dir,omitempty"`
	Remove    bool     `json:"remove,omitempty"`
	Children  []string `json:"children,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

type specFile struct {
	FragmentSize int64       `json:"fragmentSize,omitempty"`
	Resources    []SpecEntry `json:"resources"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: pack | unpack | verify | preview | apply | rollback")
	}
	switch args[0] {
	case "pack":
		return cmdPack(args[1:])
	case "unpack":
		return cmdUnpack(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "preview":
		return cmdPreview(args[1:])
	case "apply":
		return cmdApply(args[1:])
	case "rollback":
		return cmdRollback(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func loadSpec(path string) (*specFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sf specFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, err
	}
	return &sf, nil
}

func openChain(paths []string) (*assetpack.Reader, error) {
	pkgs := make([]assetpack.ReaderAtSize, 0, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, bytes.NewReader(b))
	}
	if len(pkgs) == 1 {
		b := pkgs[0].(*bytes.Reader)
		return assetpack.OpenReaderAt(b, b.Size())
	}
	return assetpack.OpenChain(pkgs)
}

func cmdPack(args []string) error {
	fs := flag.NewFlagSet("pack", flag.ContinueOnError)
	specPath := fs.String("spec", "", "spec JSON describing resources")
	outPath := fs.String("out", "", "output package path")
	basePath := fs.String("base", "", "optional base package for incremental release")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *specPath == "" || *outPath == "" {
		return fmt.Errorf("-spec and -out are required")
	}
	sf, err := loadSpec(*specPath)
	if err != nil {
		return err
	}
	reqs := make([]assetpack.AddRequest, 0, len(sf.Resources))
	for _, e := range sf.Resources {
		req := assetpack.AddRequest{Path: e.Path, IsDir: e.Dir, Remove: e.Remove, Children: e.Children, DependsOn: e.DependsOn}
		if e.Remove {
			reqs = append(reqs, req)
			continue
		}
		if !e.Dir {
			switch {
			case e.File != "":
				data, err := os.ReadFile(e.File)
				if err != nil {
					return fmt.Errorf("read %q: %w", e.File, err)
				}
				req.Data = data
			case e.Inline != "":
				req.Data = []byte(e.Inline)
			default:
				return fmt.Errorf("resource %q needs file or inline", e.Path)
			}
		}
		reqs = append(reqs, req)
	}
	opts := assetpack.Options{FragmentSize: sf.FragmentSize}
	if *basePath != "" {
		br, err := openChain([]string{*basePath})
		if err != nil {
			return err
		}
		baseM := br.Manifest()
		opts.Base = &baseM
	}
	pkg, m, err := assetpack.Build(reqs, opts)
	if err != nil {
		return err
	}
	if err := assetpack.WriteFileAtomic(*outPath, pkg, 0o644); err != nil {
		return err
	}
	fmt.Printf("packed %d resources, %d bytes, index %s%s\n", len(m.Resources), len(pkg), m.IndexDigest[:12], incSuffix(m))
	return nil
}

func incSuffix(m *assetpack.Manifest) string {
	if m.BaseIndex != "" {
		return " (incremental of " + m.BaseIndex[:12] + ")"
	}
	return ""
}

func chainFromFlags(pkg string, bases []string) (*assetpack.Reader, error) {
	return openChain(append(append([]string{}, bases...), pkg))
}

func cmdUnpack(args []string) error {
	fs := flag.NewFlagSet("unpack", flag.ContinueOnError)
	pkgPath := fs.String("pkg", "", "package path (tip)")
	dir := fs.String("dir", "", "output directory")
	bases := multiFlag{}
	fs.Var(&bases, "base", "base package, repeat oldest..newest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pkgPath == "" || *dir == "" {
		return fmt.Errorf("-pkg and -dir are required")
	}
	r, err := chainFromFlags(*pkgPath, bases)
	if err != nil {
		return err
	}
	res, err := assetpack.Unpack(r, *dir)
	if err != nil {
		return err
	}
	fmt.Printf("unpacked %d files, %d dirs into %s\n", res.Files, res.Directories, *dir)
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	pkgPath := fs.String("pkg", "", "package path (tip)")
	bases := multiFlag{}
	fs.Var(&bases, "base", "base package, repeat oldest..newest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pkgPath == "" {
		return fmt.Errorf("-pkg is required")
	}
	r, err := chainFromFlags(*pkgPath, bases)
	if err != nil {
		return err
	}
	if err := r.VerifyAll(); err != nil {
		return err
	}
	paths := make([]string, 0, len(r.Manifest().Resources))
	for _, res := range r.Manifest().Resources {
		paths = append(paths, res.Path)
	}
	sort.Strings(paths)
	fmt.Printf("OK: %d resources verified, index %s\n", len(paths), r.Manifest().IndexDigest[:12])
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdPreview(args []string) error {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	pkgPath := fs.String("pkg", "", "package path (tip)")
	dir := fs.String("dir", "", "installation directory")
	outPath := fs.String("out", "", "write plan JSON for apply -plan")
	bases := multiFlag{}
	fs.Var(&bases, "base", "base package, repeat oldest..newest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pkgPath == "" || *dir == "" {
		return fmt.Errorf("-pkg and -dir are required")
	}
	r, err := chainFromFlags(*pkgPath, bases)
	if err != nil {
		return err
	}
	plan, err := assetpack.PreviewUpdate(r, *dir)
	if err != nil {
		return err
	}
	for _, e := range plan.SortedEntries() {
		fmt.Printf("%-10s %s", e.Change, e.Path)
		if e.Decision != "" {
			fmt.Printf(" -> %s", e.Decision)
		}
		if e.ImpactNote != "" {
			fmt.Printf("  [%s]", e.ImpactNote)
		}
		fmt.Println()
	}
	if *outPath != "" {
		b, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return err
		}
		if err := assetpack.WriteFileAtomic(*outPath, b, 0o644); err != nil {
			return err
		}
		fmt.Println("plan written to", *outPath)
	}
	return nil
}

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	pkgPath := fs.String("pkg", "", "package path (tip)")
	dir := fs.String("dir", "", "installation directory")
	planPath := fs.String("plan", "", "plan JSON from preview -out")
	decPath := fs.String("decisions", "", "decisions JSON: path -> pack|keep|remove")
	resume := fs.Bool("resume", false, "resume an interrupted apply")
	bases := multiFlag{}
	fs.Var(&bases, "base", "base package, repeat oldest..newest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("-dir is required")
	}
	if *resume {
		r, err := chainFromFlags(*pkgPath, bases)
		if err != nil {
			return err
		}
		if err := assetpack.ResumeUpdate(r, *dir); err != nil {
			return err
		}
		fmt.Println("resumed update committed")
		return nil
	}
	if *pkgPath == "" {
		return fmt.Errorf("-pkg is required")
	}
	r, err := chainFromFlags(*pkgPath, bases)
	if err != nil {
		return err
	}
	var plan *assetpack.UpdatePlan
	if *planPath != "" {
		b, err := os.ReadFile(*planPath)
		if err != nil {
			return err
		}
		var p assetpack.UpdatePlan
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		plan = &p
	} else {
		p, err := assetpack.PreviewUpdate(r, *dir)
		if err != nil {
			return err
		}
		plan = p
	}
	if *decPath != "" {
		b, err := os.ReadFile(*decPath)
		if err != nil {
			return err
		}
		var decisions map[string]string
		if err := json.Unmarshal(b, &decisions); err != nil {
			return err
		}
		for path, dec := range decisions {
			if err := plan.Decide(path, dec); err != nil {
				return err
			}
		}
	}
	if err := assetpack.ApplyUpdate(r, *dir, plan); err != nil {
		return err
	}
	fmt.Println("update committed; choices recorded under .assetpack/choices.json")
	return nil
}

func cmdRollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	dir := fs.String("dir", "", "installation directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("-dir is required")
	}
	if err := assetpack.RollbackUpdate(*dir); err != nil {
		return err
	}
	fmt.Println("rolled back; manual choices kept in .assetpack/choices.json")
	return nil
}

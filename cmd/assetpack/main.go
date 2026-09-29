package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"assetpack"
)

type specResource struct {
	Paths    []string `json:"paths"`
	Source   string   `json:"source"`
	Depends  []int    `json:"depends_on"`
	Children []struct {
		Name string `json:"name"`
		Ref  int    `json:"ref"`
	} `json:"children"`
}

type spec struct {
	Resources []specResource `json:"resources"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "pack":
		err = packCmd(os.Args[2:])
	case "list":
		err = listCmd(os.Args[2:])
	case "cat":
		err = catCmd(os.Args[2:])
	case "extract":
		err = extractCmd(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: assetpack pack|list|cat|extract ...")
}

func packCmd(args []string) error {
	fs := flag.NewFlagSet("pack", flag.ContinueOnError)
	var specPath, output, basePath string
	var version int
	fs.StringVar(&specPath, "spec", "", "JSON resource description")
	fs.StringVar(&output, "o", "", "output .dap package")
	fs.StringVar(&basePath, "base", "", "optional base package for delta")
	fs.IntVar(&version, "version", 1, "package version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if specPath == "" || output == "" {
		return fmt.Errorf("pack requires -spec and -o")
	}
	data, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	var sp spec
	if err := json.Unmarshal(data, &sp); err != nil {
		return err
	}
	root := filepath.Dir(specPath)
	inputs := make([]assetpack.InputResource, 0, len(sp.Resources))
	for _, sr := range sp.Resources {
		if sr.Source == "" {
			return fmt.Errorf("resource requires source")
		}
		payload, err := os.ReadFile(filepath.Join(root, sr.Source))
		if err != nil {
			return err
		}
		in := assetpack.InputResource{
			Paths: append([]string(nil), sr.Paths...),
			Data:  payload,
			Deps:  append([]int(nil), sr.Depends...),
		}
		for _, child := range sr.Children {
			in.Children = append(in.Children, assetpack.NestedInput{Name: child.Name, Ref: child.Ref})
		}
		inputs = append(inputs, in)
	}
	if basePath == "" {
		m, err := assetpack.BuildPackage(output, inputs)
		if err != nil {
			return err
		}
		fmt.Println(m.PackageID)
		return nil
	}
	bp, err := assetpack.Open(basePath)
	if err != nil {
		return err
	}
	defer bp.Close()
	base := bp.Manifest()
	m, err := assetpack.BuildDelta(output, &base, version, inputs)
	if err != nil {
		return err
	}
	fmt.Println(m.PackageID)
	return nil
}

func listCmd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: assetpack list package.dap")
	}
	p, err := assetpack.Open(args[0])
	if err != nil {
		return err
	}
	defer p.Close()
	m := p.Manifest()
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

func catCmd(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: assetpack cat package.dap logical/path")
	}
	p, err := assetpack.Open(args[0])
	if err != nil {
		return err
	}
	defer p.Close()
	data, err := p.ReadPath(args[1])
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}

func extractCmd(args []string) error {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	var root string
	var overwrite bool
	fs.StringVar(&root, "root", "", "destination directory")
	fs.BoolVar(&overwrite, "overwrite", false, "overwrite regular files")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return fmt.Errorf("usage: assetpack extract -root DIR package.dap")
	}
	if root == "" {
		return fmt.Errorf("extract requires -root")
	}
	p, err := assetpack.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer p.Close()
	return p.Extract(assetpack.ExtractOptions{Root: root, Overwrite: overwrite})
}

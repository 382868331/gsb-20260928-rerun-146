package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"assetpack"
)

type choiceFile struct {
	Choices map[string]assetpack.UpdateChoice `json:"choices"`
	Reasons map[string]string                 `json:"reasons"`
}

func loadChoices(path string) (map[string]assetpack.UpdateChoice, map[string]string, error) {
	if path == "" {
		return nil, nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var cf choiceFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, nil, err
	}
	return cf.Choices, cf.Reasons, nil
}

func cmdPreview(args []string) error {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	pkgPath := fs.String("pkg", "", "incremental package path (tip)")
	dir := fs.String("dir", "", "installed directory")
	out := fs.String("out", "", "write preview JSON to this file")
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
	pv, err := assetpack.PreviewUpdate(r, *dir)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(pv, "", "  ")
	if err != nil {
		return err
	}
	if *out != "" {
		return assetpack.WriteFileAtomic(*out, b, 0o644)
	}
	_, err = os.Stdout.Write(append(b, '\n'))
	return err
}

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	pkgPath := fs.String("pkg", "", "incremental package path (tip)")
	dir := fs.String("dir", "", "installed directory")
	previewPath := fs.String("preview", "", "preview JSON produced before adoption")
	choicePath := fs.String("choices", "", "optional choices JSON")
	bases := multiFlag{}
	fs.Var(&bases, "base", "base package, repeat oldest..newest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pkgPath == "" || *dir == "" || *previewPath == "" {
		return fmt.Errorf("-pkg, -dir and -preview are required")
	}
	r, err := chainFromFlags(*pkgPath, bases)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(*previewPath)
	if err != nil {
		return err
	}
	var pv assetpack.UpdatePreview
	if err := json.Unmarshal(b, &pv); err != nil {
		return err
	}
	choices, reasons, err := loadChoices(*choicePath)
	if err != nil {
		return err
	}
	if err := assetpack.DecideUpdateWithReasons(&pv, choices, reasons); err != nil {
		return err
	}
	if err := assetpack.ApplyUpdate(r, *dir, &pv); err != nil {
		return err
	}
	fmt.Printf("update %s adopted into %s\n", pv.PackageIndex[:12], *dir)
	return nil
}

func cmdResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	pkgPath := fs.String("pkg", "", "incremental package path (tip)")
	dir := fs.String("dir", "", "installed directory")
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
	if err := assetpack.ResumeUpdate(r, *dir); err != nil {
		return err
	}
	fmt.Printf("pending update resumed and adopted into %s\n", *dir)
	return nil
}

func cmdWithdraw(args []string) error {
	fs := flag.NewFlagSet("withdraw", flag.ContinueOnError)
	dir := fs.String("dir", "", "installed directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("-dir is required")
	}
	if err := assetpack.WithdrawUpdate(*dir); err != nil {
		return err
	}
	fmt.Printf("latest update withdrawn from %s; human choices remain recorded\n", *dir)
	return nil
}

func cmdDecisions(args []string) error {
	fs := flag.NewFlagSet("decisions", flag.ContinueOnError)
	dir := fs.String("dir", "", "installed directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("-dir is required")
	}
	history, err := assetpack.ListUpdateHistory(*dir)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(b, '\n'))
	return err
}

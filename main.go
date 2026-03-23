package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

const (
	cR = "\033[0m"
	cO = "\033[32m"
	cI = "\033[36m"
	cP = "/etc/sert/configuration.yaml"
)

type FileDef struct {
	Content string      `yaml:"content"`
	Perm    os.FileMode `yaml:"perm"`
}

type Config struct {
	System struct {
		Timezone string `yaml:"timezone"`
	} `yaml:"system"`
	Imports   []string               `yaml:"imports"`
	Packages  []string               `yaml:"packages"`
	Flatpaks  []string               `yaml:"flatpak-apps"`
	Variables map[string]interface{} `yaml:"variables"`
	Files     map[string]FileDef     `yaml:"files"`
}

func run(show bool, args ...string) string {
	cmd := exec.Command(args[0], args[1:]...)
	if show {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Run()
		return ""
	}
	out, _ := cmd.Output()
	return string(out)
}

func loadConfig(path string, vis map[string]bool) (Config, error) {
	var c Config
	p, _ := filepath.Abs(filepath.Clean(path))
	if vis[p] { return c, nil }
	vis[p] = true
	d, err := os.ReadFile(p)
	if err != nil { return c, err }
	yaml.Unmarshal(d, &c)
	if c.Variables == nil { c.Variables = make(map[string]interface{}) }
	if c.Files == nil { c.Files = make(map[string]FileDef) }
	dir := filepath.Dir(p)
	for _, i := range c.Imports {
		if !filepath.IsAbs(i) { i = filepath.Join(dir, i) }
		if sc, err := loadConfig(i, vis); err == nil {
			if sc.System.Timezone != "" && c.System.Timezone == "" { c.System.Timezone = sc.System.Timezone }
			c.Packages = append(c.Packages, sc.Packages...)
			c.Flatpaks = append(c.Flatpaks, sc.Flatpaks...)
			for k, v := range sc.Variables { c.Variables[k] = v }
			for k, v := range sc.Files { c.Files[k] = v }
		}
	}
	return c, nil
}

func main() {
	if os.Geteuid() != 0 {
		fmt.Println("Error: root privileges required")
		os.Exit(1)
	}
	cfg, err := loadConfig(cP, make(map[string]bool))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	// 1. Timezone
	if cfg.System.Timezone != "" {
		src := filepath.Join("/usr/share/zoneinfo", cfg.System.Timezone)
		if _, err := os.Stat(src); err == nil {
			os.Remove("/etc/localtime")
			os.Symlink(src, "/etc/localtime")
			fmt.Printf("%sTIME:%s Set to %s\n", cI, cR, cfg.System.Timezone)
		}
	}

	// 2. APK Packages
	for _, p := range cfg.Packages {
		if run(false, "apk", "info", "-e", p) == "" {
			fmt.Printf("%sPKG:%s Installing %s...\n", cI, cR, p)
			run(true, "apk", "add", p)
		}
	}

	// 3. Flatpak
	if len(cfg.Flatpaks) > 0 {
		run(false, "flatpak", "remote-add", "--if-not-exists", "flathub", "https://flathub.org/repo/flathub.flatpakrepo")
		for _, f := range cfg.Flatpaks {
			fmt.Printf("%sFLAT:%s Installing %s...\n", cI, cR, f)
			run(true, "flatpak", "install", "-y", "flathub", f)
		}
	}

	// 4. Files & Templates
	userName := fmt.Sprintf("%v", cfg.Variables["user"])
	funcMap := template.FuncMap{
		"trim": func(s string) string { return strings.TrimPrefix(s, "#") },
	}

	for p, d := range cfg.Files {
		tPath := strings.ReplaceAll(p, "~/", "/home/"+userName+"/")
		os.MkdirAll(filepath.Dir(tPath), 0755)

		tmpl, err := template.New("file").Funcs(funcMap).Parse(d.Content)
		if err != nil {
			fmt.Printf("%sERR:%s Template error in %s: %v\n", cI, cR, tPath, err)
			continue
		}

		var b bytes.Buffer
		if err := tmpl.Execute(&b, cfg.Variables); err != nil {
			fmt.Printf("%sERR:%s Execute error in %s: %v\n", cI, cR, tPath, err)
			continue
		}

		perm := d.Perm
		if perm == 0 { perm = 0644 }
		os.WriteFile(tPath, b.Bytes(), perm)
		fmt.Printf("%sFILE:%s %s\n", cI, cR, tPath)

		if strings.HasPrefix(tPath, "/home/"+userName) {
			run(false, "chown", "-R", userName+":"+userName, filepath.Join("/home", userName))
		}
	}
	fmt.Printf("\n%sDONE:%s Reconfigured\n", cO, cR)
}

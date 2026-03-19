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
	cW = "\033[33m"
	cI = "\033[36m"
	cP = "/etc/sert/configuration.yaml"
)

type FileDef struct {
	Content string      `yaml:"content"`
	Owner   string      `yaml:"owner"`
	Perm    os.FileMode `yaml:"perm"`
}

type UserDef struct {
	FullName string   `yaml:"full_name"`
	Groups   []string `yaml:"groups"`
	Shell    string   `yaml:"shell"`
}

type SystemDef struct {
	Hostname string            `yaml:"hostname"`
	Timezone string            `yaml:"timezone"`
	Env      map[string]string `yaml:"env"`
}

type BootDef struct {
	User   string `yaml:"user"`
	Script string `yaml:"script"`
}

type Config struct {
	System    SystemDef              `yaml:"system"`
	Boot      BootDef                `yaml:"boot"`
	Users     map[string]UserDef     `yaml:"users"`
	Imports   []string               `yaml:"imports"`
	Packages  []string               `yaml:"packages"`
	Services  []string               `yaml:"services"`
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

func u(s []string) []string {
	m, l := make(map[string]bool), make([]string, 0, len(s))
	for _, e := range s {
		if !m[e] { m[e], l = true, append(l, e) }
	}
	return l
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
	if c.Users == nil { c.Users = make(map[string]UserDef) }
	dir := filepath.Dir(p)
	for _, i := range c.Imports {
		if !filepath.IsAbs(i) { i = filepath.Join(dir, i) }
		if sc, err := loadConfig(i, vis); err == nil {
			if sc.System.Hostname != "" && c.System.Hostname == "" { c.System.Hostname = sc.System.Hostname }
			if sc.System.Timezone != "" && c.System.Timezone == "" { c.System.Timezone = sc.System.Timezone }
			c.Packages = append(c.Packages, sc.Packages...)
			c.Services = append(c.Services, sc.Services...)
			c.Flatpaks = append(c.Flatpaks, sc.Flatpaks...)
			for k, v := range sc.Variables { c.Variables[k] = v }
			for k, v := range sc.Files { c.Files[k] = v }
			for k, v := range sc.Users { c.Users[k] = v }
			if sc.Boot.Script != "" && c.Boot.Script == "" { c.Boot = sc.Boot }
		}
	}
	c.Packages, c.Services, c.Flatpaks = u(c.Packages), u(c.Services), u(c.Flatpaks)
	return c, nil
}

func main() {
	if os.Geteuid() != 0 {
		fmt.Println("Error: root privileges required")
		os.Exit(1)
	}
	if len(os.Args) < 2 || os.Args[1] != "reconf" {
		fmt.Println("Usage: sert reconf")
		os.Exit(1)
	}
	cfg, err := loadConfig(cP, make(map[string]bool))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	funcMap := template.FuncMap{
		"stringsTrimPrefix": func(prefix string, s interface{}) string {
			return strings.TrimPrefix(fmt.Sprintf("%v", s), prefix)
		},
	}

	if cfg.System.Hostname != "" {
		os.WriteFile("/etc/hostname", []byte(cfg.System.Hostname+"\n"), 0644)
		run(false, "hostname", cfg.System.Hostname)
	}

	if cfg.System.Timezone != "" {
		src := filepath.Join("/usr/share/zoneinfo", cfg.System.Timezone)
		if _, err := os.Stat(src); err == nil {
			os.Remove("/etc/localtime")
			os.Symlink(src, "/etc/localtime")
		}
	}

	for name, def := range cfg.Users {
		g := strings.Join(def.Groups, ",")
		if run(false, "id", "-u", name) == "" {
			run(true, "useradd", "-m", "-c", def.FullName, "-s", def.Shell, "-G", g, name)
		} else {
			run(false, "usermod", "-s", def.Shell, "-G", g, name)
		}
	}

	for _, p := range cfg.Packages {
		if run(false, "apk", "info", "-e", p) == "" {
			fmt.Printf("%sPKG:%s Installing %s...\n", cI, cR, p)
			run(true, "apk", "add", p)
		}
	}

	for _, s := range cfg.Services {
		src := filepath.Join("/etc/dinit.d", s)
		dst := filepath.Join("/etc/dinit.d/boot.d", s)
		if _, err := os.Stat(src); err == nil {
			if _, err := os.Lstat(dst); os.IsNotExist(err) {
				fmt.Printf("%sSVC:%s Enabling %s...\n", cI, cR, s)
				os.Symlink(src, dst)
			}
			run(false, "dinitctl", "start", s)
		}
	}

	for _, app := range cfg.Flatpaks {
		run(true, "flatpak", "install", "-y", "flathub", app)
	}

	for p, d := range cfg.Files {
		pt, _ := template.New("path").Parse(p)
		var pb bytes.Buffer
		pt.Execute(&pb, cfg.Variables)
		t := pb.String()
		if strings.Contains(t, "<no value>") { continue }

		if strings.HasPrefix(t, "~/") {
			u := fmt.Sprintf("%v", cfg.Variables["user"])
			t = filepath.Join("/home", u, t[2:])
		}

		os.MkdirAll(filepath.Dir(t), 0755)
		tmpl, _ := template.New("file").Funcs(funcMap).Parse(d.Content)
		var b bytes.Buffer
		tmpl.Execute(&b, cfg.Variables)

		perm := d.Perm
		if perm == 0 { perm = 0644 }
		os.WriteFile(t, b.Bytes(), perm)
		fmt.Printf("%sFILE:%s %s\n", cI, cR, t)

		owner := d.Owner
		if owner == "" && strings.HasPrefix(t, "/home/") {
			u := fmt.Sprintf("%v", cfg.Variables["user"])
			owner = u + ":" + u
		} else if owner != "" {
			ot, _ := template.New("owner").Parse(owner)
			var ob bytes.Buffer
			ot.Execute(&ob, cfg.Variables)
			owner = ob.String()
		}

		if owner != "" {
			run(false, "chown", owner, t)
			curr := filepath.Dir(t)
			for strings.HasPrefix(curr, "/home/") {
				run(false, "chown", owner, curr)
				curr = filepath.Dir(curr)
				if curr == "/home" || curr == "/" { break }
			}
		}
	}
	fmt.Printf("\n%sDONE:%s System reconfigured\n", cO, cR)
}

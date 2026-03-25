package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"

	"gopkg.in/yaml.v3"
)

const (
	cR             = "\033[0m"
	cG             = "\033[32m"
	cC             = "\033[36m"
	cY             = "\033[33m"
	cRed           = "\033[31m"
	cBold          = "\033[1m"
	mainConfigPath = "/etc/sert/configuration.yaml"
	cacheDir       = "/var/cache/sert"
	stagingDir     = "/var/cache/sert/tmp"
	dinitDir       = "/etc/dinit.d"
	dinitBootDir   = "/etc/dinit.d/boot.d"
)

type FileDef struct {
	Content string      `yaml:"content"`
	Perm    os.FileMode `yaml:"perm"`
}

type RawLink struct {
	Src string `yaml:"src"`
	Dst string `yaml:"dst"`
}

type UserDef struct {
	UID         int      `yaml:"uid"`
	Groups      []string `yaml:"groups"`
	Shell       string   `yaml:"shell"`
	Home        string   `yaml:"home"`
	DotfilesSrc string   `yaml:"dotfiles_src"`
}

type AutologinDef struct {
	TTY    string `yaml:"tty"`
	User   string `yaml:"user"`
	Script string `yaml:"script"`
}

type Config struct {
	System struct {
		Timezone string `yaml:"timezone"`
		Hostname string `yaml:"hostname"`
	} `yaml:"system"`
	Imports   []string               `yaml:"imports"`
	Packages  []string               `yaml:"packages"`
	Flatpaks  []string               `yaml:"flatpak-apps"`
	Variables map[string]interface{} `yaml:"variables"`
	Files     map[string]FileDef     `yaml:"files"`
	RawLinks  []RawLink              `yaml:"raw-links"`
	Users     map[string]UserDef     `yaml:"users"`
	Services  []string               `yaml:"services"`
	Autologin *AutologinDef          `yaml:"autologin"`
}

func logOK(tag, msg string)   { fmt.Printf("%s%s%-6s%s %s\n", cBold, cG, tag+":", cR, msg) }
func logInfo(tag, msg string) { fmt.Printf("%s%s%-6s%s %s\n", cBold, cC, tag+":", cR, msg) }
func logWarn(msg string)      { fmt.Printf("%s%sWARN:  %s %s\n", cBold, cY, cR, msg) }
func logErr(msg string)       { fmt.Printf("%s%sERR:    %s %s\n", cBold, cRed, cR, msg) }

func run(show bool, args ...string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	var stdout, stderr bytes.Buffer
	if show {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(stderr.String()), err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func mustRun(show bool, args ...string) {
	if _, err := run(show, args...); err != nil {
		logErr(fmt.Sprintf("Command %v failed: %v", args, err))
	}
}

func loadConfig(path string, visited map[string]bool) (Config, error) {
	var c Config
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return c, err
	}
	if visited[abs] {
		return c, nil
	}
	visited[abs] = true
	data, err := os.ReadFile(abs)
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.Variables == nil {
		c.Variables = make(map[string]interface{})
	}
	if c.Files == nil {
		c.Files = make(map[string]FileDef)
	}
	if c.Users == nil {
		c.Users = make(map[string]UserDef)
	}
	dir := filepath.Dir(abs)
	for _, imp := range c.Imports {
		if !filepath.IsAbs(imp) {
			imp = filepath.Join(dir, imp)
		}
		sub, err := loadConfig(imp, visited)
		if err != nil {
			logWarn(fmt.Sprintf("Skipping import %q: %v", imp, err))
			continue
		}
		if sub.System.Timezone != "" && c.System.Timezone == "" {
			c.System.Timezone = sub.System.Timezone
		}
		if sub.System.Hostname != "" && c.System.Hostname == "" {
			c.System.Hostname = sub.System.Hostname
		}
		c.Packages = append(c.Packages, sub.Packages...)
		c.Flatpaks = append(c.Flatpaks, sub.Flatpaks...)
		c.RawLinks = append(c.RawLinks, sub.RawLinks...)
		c.Services = append(c.Services, sub.Services...)
		if sub.Autologin != nil && c.Autologin == nil {
			c.Autologin = sub.Autologin
		}
		for k, v := range sub.Variables {
			c.Variables[k] = v
		}
		for k, v := range sub.Files {
			c.Files[k] = v
		}
		for k, v := range sub.Users {
			c.Users[k] = v
		}
	}
	return c, nil
}

func dedup(s []string) []string {
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

func userExists(name string) bool {
	_, err := run(false, "id", "-u", name)
	return err == nil
}

func groupExists(name string) bool {
	_, err := run(false, "getent", "group", name)
	return err == nil
}

func applyUsers(users map[string]UserDef) {
	for name, u := range users {
		if !userExists(name) {
			args := []string{"useradd", "-m"}
			if u.UID > 0 {
				args = append(args, "-u", strconv.Itoa(u.UID))
			}
			if u.Shell != "" {
				args = append(args, "-s", u.Shell)
			}
			if u.Home != "" {
				args = append(args, "-d", u.Home)
			}
			args = append(args, name)
			mustRun(false, args...)
			logOK("USER", "Created "+name)
		}
		for _, g := range u.Groups {
			if !groupExists(g) {
				mustRun(false, "groupadd", g)
			}
			mustRun(false, "usermod", "-aG", g, name)
			logOK("GROUP", fmt.Sprintf("%s -> %s", name, g))
		}
	}
}

func applyTimezone(tz string) {
	src := filepath.Join("/usr/share/zoneinfo", tz)
	if _, err := os.Stat(src); err != nil {
		return
	}
	os.Remove("/etc/localtime")
	os.Symlink(src, "/etc/localtime")
	logOK("TIME", tz)
}

func applyHostname(name string) {
	os.WriteFile("/etc/hostname", []byte(name+"\n"), 0644)
	mustRun(false, "hostname", name)
	logOK("HOST", name)
}

func applyPackages(pkgs []string) []string {
	var failed []string
	repoFile := "/etc/apk/repositories"
	data, err := os.ReadFile(repoFile)
	if err == nil {
		content := string(data)
		if strings.Contains(content, "#http") {
			newContent := strings.ReplaceAll(content, "#http", "http")
			os.WriteFile(repoFile, []byte(newContent), 0644)
			logInfo("REPO", "Enabled all branches")
			mustRun(true, "apk", "update")
		}
	}

	if len(pkgs) == 0 {
		return failed
	}
	pkgs = dedup(pkgs)
	
	for _, p := range pkgs {
		out, _ := run(false, "apk", "info", "-e", p)
		if out == "" {
			logInfo("PKG", "Installing "+p+"...")
			_, err := run(true, "apk", "add", p)
			if err != nil {
				logErr("Failed to install " + p)
				failed = append(failed, p)
			} else {
				logOK("PKG", p+" installed")
			}
		}
	}
	return failed
}

func applyFlatpaks(apps []string) {
	if len(apps) == 0 {
		return
	}
	apps = dedup(apps)
	run(false, "flatpak", "remote-add", "--if-not-exists", "flathub", "https://flathub.org/repo/flathub.flatpakrepo")
	for _, f := range apps {
		mustRun(true, "flatpak", "install", "-y", "flathub", f)
	}
}

func renderAtomic(target string, content string, vars map[string]interface{}, perm os.FileMode) (bool, error) {
	fm := template.FuncMap{
		"trim":    func(s string) string { return strings.TrimPrefix(s, "#") },
		"upper":    strings.ToUpper,
		"lower":    strings.ToLower,
		"replace": strings.ReplaceAll,
		"default": func(def, val interface{}) interface{} {
			if val == nil || val == "" {
				return def
			}
			return val
		},
	}
	tmpl, err := template.New("").Funcs(fm).Parse(content)
	if err != nil {
		return false, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return false, err
	}
	os.MkdirAll(stagingDir, 0700)
	tmpFile := filepath.Join(stagingDir, filepath.Base(target)+".tmp")
	if err := os.WriteFile(tmpFile, buf.Bytes(), perm); err != nil {
		return false, err
	}
	old, _ := os.ReadFile(target)
	if bytes.Equal(old, buf.Bytes()) {
		os.Remove(tmpFile)
		return false, nil
	}
	os.MkdirAll(filepath.Dir(target), 0755)
	if err := os.Rename(tmpFile, target); err != nil {
		return false, err
	}
	return true, nil
}

func expandPath(p, userName string) string {
	p = strings.ReplaceAll(p, "~/", "/home/"+userName+"/")
	p = strings.ReplaceAll(p, "$HOME", "/home/"+userName)
	return p
}

func applyFiles(files map[string]FileDef, vars map[string]interface{}) bool {
	userName := fmt.Sprintf("%v", vars["user"])
	changedAny := false
	var wg sync.WaitGroup
	var mu sync.Mutex
	for path, def := range files {
		wg.Add(1)
		go func(p string, d FileDef) {
			defer wg.Done()
			target := expandPath(p, userName)
			perm := d.Perm
			if perm == 0 {
				perm = 0644
			}
			changed, err := renderAtomic(target, d.Content, vars, perm)
			if err != nil {
				logErr(fmt.Sprintf("File %s: %v", target, err))
				return
			}
			if changed {
				mu.Lock()
				changedAny = true
				mu.Unlock()
				logOK("FILE", target)
			}
		}(path, def)
	}
	wg.Wait()
	return changedAny
}

func applyServices(services []string, forceRestart bool) {
	os.MkdirAll(dinitBootDir, 0755)
	for _, svc := range dedup(services) {
		svcFile := filepath.Join(dinitDir, svc)
		bootLink := filepath.Join(dinitBootDir, svc)
		if _, err := os.Stat(svcFile); err != nil {
			continue
		}
		isNew := false
		if _, err := os.Lstat(bootLink); err != nil {
			os.Symlink(svcFile, bootLink)
			logOK("SVC", "Enabled "+svc)
			isNew = true
		}
		status, _ := run(false, "dinitctl", "status", svc)
		isRunning := strings.Contains(status, "Status: started")
		if isNew || !isRunning {
			run(false, "dinitctl", "start", svc)
		} else if forceRestart {
			run(false, "dinitctl", "restart", svc)
			logOK("RESTART", svc)
		}
	}
}

func applyAutologin(al *AutologinDef) {
	if al == nil || al.TTY == "" || al.User == "" {
		return
	}
	svcName := "getty-" + al.TTY
	content := fmt.Sprintf("type = process\ncommand = /sbin/agetty --autologin %s --noclear %s linux\nrestart = true\n", al.User, al.TTY)
	renderAtomic(filepath.Join(dinitDir, svcName), content, nil, 0644)
	bootLink := filepath.Join(dinitBootDir, svcName)
	if _, err := os.Lstat(bootLink); err != nil {
		os.Symlink(filepath.Join(dinitDir, svcName), bootLink)
	}
	logOK("LOGIN", al.User)
}

func cmdReconf() {
	if os.Geteuid() != 0 {
		logErr("Root privileges required")
		os.Exit(1)
	}
	cfg, err := loadConfig(mainConfigPath, make(map[string]bool))
	if err != nil {
		logErr(err.Error())
		os.Exit(1)
	}
	if cfg.System.Hostname != "" {
		applyHostname(cfg.System.Hostname)
	}
	if cfg.System.Timezone != "" {
		applyTimezone(cfg.System.Timezone)
	}
	applyUsers(cfg.Users)
	
	failedPkgs := applyPackages(cfg.Packages)
	
	applyFlatpaks(cfg.Flatpaks)
	changed := applyFiles(cfg.Files, cfg.Variables)
	applyServices(cfg.Services, changed)
	applyAutologin(cfg.Autologin)
	
	if len(failedPkgs) > 0 {
		fmt.Printf("\n%s%s%sThe following packages failed to install:%s\n", cBold, cRed, strings.Repeat("-", 10), cR)
		for _, p := range failedPkgs {
			fmt.Printf("  - %s\n", p)
		}
		fmt.Println()
	}
	
	logOK("DONE", "System reconfigured")
}

func cmdUpdate() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); mustRun(true, "apk", "upgrade") }()
	go func() { defer wg.Done(); mustRun(true, "flatpak", "update", "-y") }()
	wg.Wait()
}

func cmdStart() {
	target := "configuration.yaml"
	defaultYaml := "system:\n  hostname: chimera\nusers:\n  alice:\n    uid: 1000\n    groups: [wheel]\nservices:\n  - dbus"
	os.WriteFile(target, []byte(defaultYaml), 0644)
	logOK("START", target)
}

func usage() {
	fmt.Printf("sert — declarative config\n\nCommands:\n  start, reconf, update\n")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	switch os.Args[1] {
	case "start":
		cmdStart()
	case "reconf":
		cmdReconf()
	case "update":
		cmdUpdate()
	default:
		usage()
	}
}

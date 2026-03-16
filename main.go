package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorCyan   = "\033[36m"
)

type Config struct {
	System struct {
		Hostname string `yaml:"hostname"`
	} `yaml:"system"`
	Packages []string `yaml:"packages"`
}

func pkgExists(pkg string) bool {
	cmd := exec.Command("apk", "search", "-v", pkg)
	out, _ := cmd.Output()
	return strings.Contains(string(out), pkg)
}

func isInstalled(pkg string) bool {
	cmd := exec.Command("apk", "info", "-e", pkg)
	return cmd.Run() == nil
}

func installPackage(pkg string) {
	if !pkgExists(pkg) {
		fmt.Printf("%sERROR:%s Package '%s' not found in repositories\n", ColorRed, ColorReset, pkg)
		return
	}

	fmt.Printf("%sINFO:%s Installing %s...\n", ColorCyan, ColorReset, pkg)
	cmd := exec.Command("doas", "apk", "add", pkg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err == nil {
		fmt.Printf("%sOK:%s %s installed successfully\n", ColorGreen, ColorReset, pkg)
	}
}

func removeUnused(allowed []string) {
	data, err := os.ReadFile("/etc/apk/world")
	if err != nil {
		fmt.Printf("%sERROR:%s Could not read world file: %v\n", ColorRed, ColorReset, err)
		return
	}

	installedInWorld := strings.Fields(string(data))
	allowedMap := make(map[string]bool)
	for _, p := range allowed {
		allowedMap[p] = true
	}

	for _, pkg := range installedInWorld {
		if !allowedMap[pkg] {
			fmt.Printf("%sCLEAN:%s Removing %s...\n", ColorYellow, ColorReset, pkg)
			cmd := exec.Command("doas", "apk", "del", pkg)
			cmd.Run()
		}
	}
}

func main() {
	f, err := os.Open("/etc/sert/configuration.yaml")
	if err != nil {
		fmt.Printf("%sFATAL:%s %v\n", ColorRed, ColorReset, err)
		return
	}
	defer f.Close()

	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		fmt.Printf("%sERROR:%s YAML decoding failed: %v\n", ColorRed, ColorReset, err)
		return
	}

	fmt.Printf("Sert System: %s\n", cfg.System.Hostname)

	for _, pkg := range cfg.Packages {
		if !isInstalled(pkg) {
			installPackage(pkg)
		}
	}

	removeUnused(cfg.Packages)
	fmt.Printf("\n%sDONE:%s System state synchronized\n", ColorGreen, ColorReset)
}

// Copyright 2023 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// runvmtest sets VMTEST_QEMU and VMTEST_KERNEL (if not already set) with
// binaries downloaded from Docker images, then executes a command.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"text/template"

	"dagger.io/dagger"
)

var (
	keepArtifacts = flag.Bool("keep-artifacts", false, "Keep artifacts directory available for further local tests")
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}

// TestEnvConfig is a map of container name -> env var name -> variable config.
type TestEnvConfig map[string]map[string]EnvVar

// ArchTestConfig is a map of GOARCH -> config for env vars to set up.
type ArchTestConfig map[string]TestEnvConfig

type EnvVar struct {
	// Template uses text/template syntax and is evaluated to become the env var.
	//
	// {{.Files.$name}} can be used to refer to files extracted from the
	// container, where $name is the key to one of the Files / Directories
	// maps.
	Template string

	// Map of template variable name -> path in container
	Files map[string]string

	// Map of template variable name -> path in container
	Directories map[string]string
}

var configs = ArchTestConfig{
	"amd64": {
		"ghcr.io/hugelgupf/vmtest/kernel-amd64:main": map[string]EnvVar{
			"VMTEST_KERNEL": EnvVar{
				Template: "{{.Files.bzImage}}",
				Files:    map[string]string{"bzImage": "/bzImage"},
			},
		},
		"ghcr.io/hugelgupf/vmtest/qemu:main": map[string]EnvVar{
			"VMTEST_QEMU": EnvVar{
				Template:    "{{.Files.qemu}}/bin/qemu-system-x86_64 -L {{.Files.qemu}}/pc-bios -m 1G",
				Directories: map[string]string{"qemu": "/zqemu"},
			},
		},
	},
	"arm": {
		"ghcr.io/hugelgupf/vmtest/kernel-arm:main": map[string]EnvVar{
			"VMTEST_KERNEL": EnvVar{
				Template: "{{.Files.zImage}}",
				Files:    map[string]string{"zImage": "/zImage"},
			},
		},
		"ghcr.io/hugelgupf/vmtest/qemu:main": map[string]EnvVar{
			"VMTEST_QEMU": EnvVar{
				Template:    "{{.Files.qemu}}/bin/qemu-system-arm -M virt,highmem=off -L {{.Files.qemu}}/pc-bios",
				Directories: map[string]string{"qemu": "/zqemu"},
			},
		},
	},
	"arm64": {
		"ghcr.io/hugelgupf/vmtest/kernel-arm64:main": map[string]EnvVar{
			"VMTEST_KERNEL": EnvVar{
				Template: "{{.Files.Image}}",
				Files:    map[string]string{"Image": "/Image"},
			},
		},
		"ghcr.io/hugelgupf/vmtest/qemu:main": map[string]EnvVar{
			"VMTEST_QEMU": EnvVar{
				Template:    "{{.Files.qemu}}/bin/qemu-system-aarch64 -machine virt -cpu max -m 1G -L {{.Files.qemu}}/pc-bios",
				Directories: map[string]string{"qemu": "/zqemu"},
			},
		},
	},
}

func defaultConfig() TestEnvConfig {
	arch := os.Getenv("VMTEST_ARCH")
	if c, ok := configs[arch]; ok {
		return c
	}
	if c, ok := configs[runtime.GOARCH]; ok {
		return c
	}
	// On other architectures, user has to provide all values via flags.
	return TestEnvConfig{}
}

func run() error {
	config := defaultConfig()
	//config.RegisterFlags(flag.CommandLine)
	flag.Parse()

	if flag.NArg() < 2 {
		return fmt.Errorf("too few arguments: usage: `%s -- ./test-to-run`", os.Args[0])
	}

	ctx := context.Background()
	client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stdout))
	if err != nil {
		return fmt.Errorf("unable to connect to client: %w", err)
	}
	defer client.Close()

	return runNatively(ctx, client, config, flag.Args())
}

func runNatively(ctx context.Context, client *dagger.Client, config TestEnvConfig, args []string) error {
	var tmpDir string

	if !*keepArtifacts {
		c := make(chan os.Signal, 1)
		defer close(c)

		signal.Notify(c, os.Interrupt)
		defer signal.Stop(c)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-c:
				os.RemoveAll(tmpDir)

			case <-ctx.Done():
				return
			}
		}()

		defer wg.Wait()
	}

	// Cancel before wg.Wait(), so goroutine can exit.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tmpDir, err := os.MkdirTemp(".", "ci-testing")
	if err != nil {
		return fmt.Errorf("unable to create tmp dir: %w", err)
	}
	if !*keepArtifacts {
		defer os.RemoveAll(tmpDir)
	}
	tmp, err := filepath.Abs(tmpDir)
	if err != nil {
		return fmt.Errorf("could not retrieve absolute path: %w", err)
	}

	base := client.Container()
	var envv []string
	for containerName, envs := range config {
		for varName, varConf := range envs {
			// Already set by caller.
			if os.Getenv(varName) != "" {
				continue
			}

			files := struct {
				Files map[string]string
			}{
				Files: make(map[string]string),
			}
			for templateName, file := range varConf.Files {
				base = base.WithFile(file, client.Container().From(containerName).File(file))
				files.Files[templateName] = filepath.Join(tmp, file)
			}
			for templateName, dir := range varConf.Directories {
				base = base.WithDirectory(dir, client.Container().From(containerName).Directory(dir))
				files.Files[templateName] = filepath.Join(tmp, dir)
			}

			tmpl, err := template.New("var-" + varName).Parse(varConf.Template)
			if err != nil {
				return fmt.Errorf("invalid %s template: %w", varName, err)
			}
			var s strings.Builder
			if err := tmpl.Execute(&s, files); err != nil {
				return fmt.Errorf("failed to substitute %s template variables: %w", varName, err)
			}
			envv = append(envv, varName+"="+s.String())
		}
	}
	artifacts := base.Directory("/")

	if ok, err := artifacts.Export(ctx, tmpDir); !ok || err != nil {
		return fmt.Errorf("failed artifact export: %w", err)
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), envv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if *keepArtifacts {
		defer func() {
			fmt.Println("\nTo run another test using the same artifacts:")

			fmt.Printf("%s ...\n", strings.Join(envv, " "))
		}()
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed execution: %w", err)
	}
	return nil
}

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/cockroachdb/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"encr.dev/internal/version"
)

// A DistBuilder is a builder for a specific distribution of Encore.
//
// Anything which does not need to be built for a specific distribution
// should be built in the main builder before these are invoked.
//
// Make release will run multiple of these in parallel to build all the
// distributions.
type DistBuilder struct {
	log              zerolog.Logger
	OS               string      // The OS to build for
	Arch             string      // The architecture to build for
	TSParserPath     string      // The path to the ts-parser repo
	DistBuildDir     string      // The directory to build into
	ArtifactsTarFile string      // The directory to put the final tar.gz artifact into
	Version          string      // The version to build
	jsBuilder        *JSPackager // The JS builder
}

func (d *DistBuilder) buildEncoreCLI() error {
	// Build the CLI binaries.
	d.log.Info().Msg("building encore binary...")

	linkerOpts := []string{
		"-X", fmt.Sprintf("'encr.dev/internal/version.Version=%s'", d.Version),
	}

	// If we're building a nightly, devel or beta version, we need to set the default config directory
	var versionSuffix string
	switch version.ChannelFor(d.Version) {
	case version.GA:
		versionSuffix = ""
	case version.Beta:
		versionSuffix = "-beta"
	case version.Nightly:
		versionSuffix = "-nightly"
	case version.DevBuild:
		versionSuffix = "-develop"
	default:
		return errors.Newf("unknown version channel for %s", d.Version)
	}

	if versionSuffix != "" {
		linkerOpts = append(linkerOpts,
			"-X", "'encr.dev/internal/conf.defaultConfigDirectory=encore"+versionSuffix+"'",
		)
	}

	err := CompileGoBinary(
		join(d.DistBuildDir, "bin", "encore"+versionSuffix),
		"./cli/cmd/encore",
		linkerOpts,
		d.OS,
		d.Arch,
	)
	if err != nil {
		d.log.Err(err).Msg("encore failed to build")
		return errors.Wrap(err, "compile encore")
	}
	d.log.Info().Msg("encore built successfully")
	return nil
}

func (d *DistBuilder) buildGitHook() error {
	// Build the git-remote-encore binary.
	d.log.Info().Msg("building git-remote-encore binary...")
	err := CompileGoBinary(
		join(d.DistBuildDir, "bin", "git-remote-encore"),
		"./cli/cmd/git-remote-encore",
		nil,
		d.OS,
		d.Arch,
	)
	if err != nil {
		d.log.Err(err).Msg("git-remote-encore failed to build")
		return errors.Wrap(err, "compile git-remote-encore")
	}
	d.log.Info().Msg("git-remote-encore built successfully")
	return nil
}

func (d *DistBuilder) buildTSBundler() error {
	// Build the TS bundler.
	d.log.Info().Msg("building tsbundler binary...")

	linkerOpts := []string{
		"-X", fmt.Sprintf("'encr.dev/internal/version.Version=%s'", d.Version),
	}

	err := CompileGoBinary(
		join(d.DistBuildDir, "bin", "tsbundler-encore"),
		"./cli/cmd/tsbundler-encore",
		linkerOpts,
		d.OS,
		d.Arch,
	)
	if err != nil {
		d.log.Err(err).Msg("tsbundler failed to build")
		return errors.Wrap(err, "compile tsbundler")
	}
	d.log.Info().Msg("tsbundler built successfully")
	return nil
}

func (d *DistBuilder) buildTSParser() error {
	// Build the TS parser.
	d.log.Info().Msg("building ts-parser binary...")
	err := CompileRustBinary(
		"tsparser-encore",
		join(d.DistBuildDir, "bin", "tsparser-encore"),
		d.TSParserPath,
		d.OS,
		d.Arch,
		fmt.Sprintf("ENCORE_VERSION=%s", d.Version),
	)
	if err != nil {
		d.log.Err(err).Msg("ts-parser failed to build")
		return errors.Wrap(err, "compile ts-parser")
	}
	d.log.Info().Msg("ts-parser built successfully")
	return nil
}

func (d *DistBuilder) buildNodePlugin() error {
	d.log.Info().Msg("building node plugin...")

	// Figure out the names of the compiled and target binaries.
	compiledBinaryName, err := func() (string, error) {
		switch d.OS {
		case "darwin":
			return "libencore_js_runtime.dylib", nil
		case "linux":
			return "libencore_js_runtime.so", nil
		case "windows":
			return "encore_js_runtime.dll", nil
		default:
			return "", errors.Newf("unknown OS: %s", d.OS)
		}
	}()
	if err != nil {
		d.log.Err(err).Msg("node plugin failed to build")
		return errors.Wrap(err, "compile node plugin")
	}

	d.log.Info().Msg("Patching jscore/api/version.cjs...")
	err = os.WriteFile(
		filepath.Join(".", "runtimes", "jscore", "api", "version.cjs"),
		[]byte(`// Code generated by /pkg/make-release. DO NOT EDIT.

/**
 * The version of the runtime this JS bundle was built for
 */
module.exports.version = "`+d.Version+`";
`),
		0644,
	)
	if err != nil {
		d.log.Err(err).Msg("failed to patch version.cjs")
		return errors.Wrap(err, "write patch version.cjs")
	}

	// Build the node plugin.
	err = CompileRustBinary(
		compiledBinaryName,
		join(d.DistBuildDir, "bin", "encore-runtime.node"),
		"./runtimes/jscore",
		d.OS,
		d.Arch,
		fmt.Sprintf("ENCORE_VERSION=%s", d.Version),
	)
	if err != nil {
		d.log.Err(err).Msg("node plugin failed to build")
		return errors.Wrap(err, "compile node plugin")
	}
	d.log.Info().Msg("node plugin built successfully")
	return nil
}

func (d *DistBuilder) downloadEncoreGo() error {
	// Step 1: Find out the latest release version for Encore's Go distribution
	d.log.Info().Msg("downloading latest encore-go...")
	encoreGoArchive, err := downloadLatestGithubRelease("encoredev", "go", d.OS, d.Arch)
	if err != nil {
		d.log.Err(err).Msg("failed to download encore-go")
		return errors.Wrap(err, "download encore-go")
	}

	d.log.Info().Msg("extracting encore-go...")
	err = extractArchive(encoreGoArchive, d.DistBuildDir)
	if err != nil {
		d.log.Err(err).Msg("failed to extract encore-go")
		return errors.Wrap(err, "extract encore-go")
	}

	d.log.Info().Msg("encore-go extracted successfully")
	return nil
}

func (d *DistBuilder) copyEncoreRuntimeForGo() error {
	d.log.Info().Msg("copying encore runtime for Go...")
	cmd := exec.Command("cp", "-r", "runtimes/go/.", join(d.DistBuildDir, "runtimes", "go")+"/")
	// nosemgrep
	if out, err := cmd.CombinedOutput(); err != nil {
		d.log.Err(err).Str("stderr", string(out)).Msg("encore runtime for go failed to be copied")
		return errors.Wrapf(err, "cp go runtime: %s", out)
	}
	d.log.Info().Msg("encore runtime for go copied successfully")
	return nil
}

func (d *DistBuilder) copyEncoreRuntimeForJS() error {
	d.log.Info().Msg("waiting for JS packager to complete...")
	<-d.jsBuilder.compileCompleted
	if d.jsBuilder.compileFailed.Load() {
		d.log.Error().Msg("JS packager failed to build")
		return errors.New("js build failed")
	}

	d.log.Info().Msg("copying encore runtime for JS...")
	cmd := exec.Command("cp", "-r", d.jsBuilder.DistFolder+"/.", join(d.DistBuildDir, "runtimes", "js")+"/")
	// nosemgrep
	if out, err := cmd.CombinedOutput(); err != nil {
		d.log.Err(err).Str("stderr", string(out)).Msg("encore runtime for js failed to be copied")
		return errors.Wrapf(err, "cp js runtime: %s", out)
	}
	d.log.Info().Msg("encore runtime for js copied successfully")
	return nil
}

// Build builds the distribution running each step in order
func (d *DistBuilder) Build() error {
	d.log = log.With().Str("os", d.OS).Str("arch", d.Arch).Logger()

	d.log.Info().Msg("building distribution...")

	// Prepare the target directory.
	if err := os.RemoveAll(d.DistBuildDir); err != nil {
		d.log.Err(err).Msg("failed to remove existing target dir")
		return errors.Wrap(err, "remove target dir")
	} else if err := os.MkdirAll(d.DistBuildDir, 0755); err != nil {
		d.log.Err(err).Msg("failed to create target dir")
		return errors.Wrap(err, "create target dir")
	} else if err := os.MkdirAll(join(d.DistBuildDir, "bin"), 0755); err != nil {
		d.log.Err(err).Msg("failed to create bin dir")
		return errors.Wrap(err, "create bin dir")
	} else if err := os.MkdirAll(join(d.DistBuildDir, "runtimes"), 0755); err != nil {
		d.log.Err(err).Msg("failed to create runtimes dir")
		return errors.Wrap(err, "create runtimes/go dir")
	} else if err := os.MkdirAll(join(d.DistBuildDir, "runtimes", "go"), 0755); err != nil {
		d.log.Err(err).Msg("failed to create runtimes/go dir")
		return errors.Wrap(err, "create runtimes/go dir")
	} else if err := os.MkdirAll(join(d.DistBuildDir, "runtimes", "js"), 0755); err != nil {
		d.log.Err(err).Msg("failed to create runtimes/js dir")
		return errors.Wrap(err, "create runtimes/js dir")
	}

	// Now we're prepped, start building.
	err := runParallel(
		d.buildEncoreCLI,
		d.buildTSBundler,
		d.buildGitHook,
		d.buildTSParser,
		d.buildNodePlugin,
		d.copyEncoreRuntimeForGo,
		d.copyEncoreRuntimeForJS,
		d.downloadEncoreGo,
	)
	if err != nil {
		d.log.Err(err).Msg("failed to build distribution")
		return errors.Wrapf(err, " os: %s, arch: %s", d.OS, d.Arch)
	}

	// Now tar gzip the directory
	d.log.Info().Str("tar_file", d.ArtifactsTarFile).Msg("creating distribution tar file...")
	err = TarGzip(d.DistBuildDir, d.ArtifactsTarFile)
	if err != nil {
		d.log.Err(err).Msg("failed to tar gzip distribution")
		return errors.Wrapf(err, " os: %s, arch: %s", d.OS, d.Arch)
	}

	d.log.Info().Str("tar_file", d.ArtifactsTarFile).Msg("distribution built successfully")
	return nil
}

// runParallel runs the given functions in parallel, returning the first error
func runParallel(functions ...func() error) error {
	var wg sync.WaitGroup
	wg.Add(len(functions))
	var firstErr error
	var mu sync.Mutex

	for _, f := range functions {
		f := f
		go func() {
			defer wg.Done()

			if err := f(); err != nil {
				mu.Lock()
				defer mu.Unlock()
				if firstErr == nil {
					firstErr = err
				}
			}
		}()
	}

	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	return firstErr
}

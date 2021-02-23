package cmd

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"hash/crc32"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daedaleanai/dbt/log"
	"github.com/daedaleanai/dbt/module"
	"github.com/daedaleanai/dbt/util"

	"github.com/daedaleanai/cobra"
)

const buildDirName = "BUILD"
const buildFileName = "BUILD.go"
const buildFilesDirName = "buildfiles"
const dbtModulePath = "github.com/daedaleanai/dbt v0.1.8"
const initFileName = "init.go"
const mainFileName = "main.go"
const modFileName = "go.mod"
const ninjaFileName = "build.ninja"
const outputDirName = "output"
const RulesDirName = "RULES"

const goVersion = "1.13"

const initFileTemplate = `
// This file is generated. Do not edit this file.

package %s

import (
	"path"
	"reflect"

	"dbt/RULES/core"
)

type __internal_pkg struct{}

func DbtMain(ctx core.Context) {
%s
}

func in(name string) core.Path {
	return core.NewInPath(path.Join(reflect.TypeOf(__internal_pkg{}).PkgPath(), name))
}

func ins(names ...string) core.Paths {
	var paths core.Paths
	for _, name := range names {
		paths = append(paths, in(name))
	}
	return paths
}

func out(name string) core.OutPath {
	return core.NewOutPath(path.Join(reflect.TypeOf(__internal_pkg{}).PkgPath(), name))
}
`

const mainFileTemplate = `
// This file is generated. Do not edit this file.

package main

import (
	"fmt"
	"os"
)

import "dbt/RULES/core"

%s

func main() {
	var ctx core.Context

	switch os.Args[1] {
	case "ninja":
		ctx = &core.NinjaContext{}
		ctx.Initialize()
	case "targets":
		ctx = &core.ListTargetsContext{}
		ctx.Initialize()
	case "flags":
		for flag := range core.BuildFlags {
			fmt.Println(flag)
		}
		return
	}

	core.LockBuildFlags()
%s
}
`

type buildInfo struct {
	workingDir     string
	sourceDir      string
	buildOutputDir string
	buildFilesDir  string
	buildFlags     []string
	targets        []string
	ninjaTargets   []string
}

var buildCmd = &cobra.Command{
	Use:                   "build [targets] [build flags]",
	Short:                 "Builds the targets",
	Long:                  `Builds the targets.`,
	Run:                   runBuild,
	ValidArgsFunction:     completeArgs,
	DisableFlagsInUseLine: true,
}

func init() {
	rootCmd.AddCommand(buildCmd)
}

func runBuild(cmd *cobra.Command, args []string) {
	info := prepareGenerator(args)

	log.Debug("Normalized targets: '%s'.\n", strings.Join(info.targets, "', '"))

	// Get all available targets and flags.
	availableTargets := getAvailableTargets(info)
	availableFlags := getAvailableFlags(info)

	if len(info.targets) == 0 {
		log.Debug("No targets specified.\n")

		fmt.Println("\nAvailable targets:")
		for target := range availableTargets {
			fmt.Printf("  //%s\n", target)
		}

		fmt.Println("\nAvailable flags:")
		for flag := range availableFlags {
			fmt.Printf("  %s=\n", flag)
		}
		return
	}

	uniqueNinjaTargets := map[string]struct{}{}
	for _, target := range info.targets {
		if !strings.HasSuffix(target, "...") {
			if _, exists := availableTargets[target]; !exists {
				log.Fatal("Target '%s' does not exist.\n", target)
			}
			uniqueNinjaTargets[target] = struct{}{}
			continue
		}

		targetPrefix := strings.TrimSuffix(target, "...")
		found := false
		for availableTarget := range availableTargets {
			if strings.HasPrefix(availableTarget, targetPrefix) {
				found = true
				uniqueNinjaTargets[availableTarget] = struct{}{}
			}
		}
		if !found {
			log.Fatal("No target is matching pattern '%s'.\n", target)
		}
	}

	for target := range uniqueNinjaTargets {
		info.ninjaTargets = append(info.ninjaTargets, target)
	}
	log.Debug("Expanded targets: '%s'.\n", strings.Join(info.ninjaTargets, "', '"))

	for _, flag := range info.buildFlags {
		name := strings.Split(flag, "=")[0]
		if _, exists := availableFlags[name]; !exists {
			log.Fatal("Flag '%s' does not exist.\n", name)
		}
	}

	// Produce the ninja.build file and run Ninja.
	runNinja(info)
}

func completeArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	info := prepareGenerator(args)

	suggestions := []string{}
	for flag := range getAvailableFlags(info) {
		suggestions = append(suggestions, fmt.Sprintf("%s=", flag))
	}

	targetToComplete := normalizeTarget(toComplete)
	numParts := len(strings.Split(targetToComplete, "/"))
	for target := range getAvailableTargets(info) {
		if !strings.HasPrefix(target, targetToComplete) {
			continue
		}
		suggestion := strings.Join(strings.SplitAfter(target, "/")[0:numParts], "")
		suggestion = toComplete + strings.TrimPrefix(suggestion, targetToComplete)
		suggestions = append(suggestions, suggestion)
	}

	return suggestions, cobra.ShellCompDirectiveNoSpace
}

func normalizeTarget(target string) string {
	// Build targets are interpreted as relative to the workspace root when they start with a '/'.
	// Otherwise they are interpreted as relative to the current working directory.
	// E.g.: Running 'dbt build //src/path/to/mylib.a' from anywhere in the workspace is equivallent
	// to running 'dbt build mylib.a' in '.../src/path/to/' or running 'dbt build path/to/mylib.a' in '.../src/'.
	if strings.HasPrefix(target, "//") {
		return strings.TrimLeft(target, "/")
	}
	endsWithSlash := strings.HasSuffix(target, "/") || target == ""
	target = path.Join(util.GetWorkingDir(), target)
	moduleRoot := util.GetModuleRootForPath(target)
	target = strings.TrimPrefix(target, path.Dir(moduleRoot))
	if endsWithSlash {
		target = target + "/"
	}
	return strings.TrimLeft(target, "/")
}

func prepareGenerator(args []string) buildInfo {
	info := buildInfo{}

	workspaceRoot := util.GetWorkspaceRoot()
	info.sourceDir = path.Join(workspaceRoot, util.DepsDirName)
	info.workingDir = util.GetWorkingDir()

	// Split all args into two categories: If they contain a "= they are considered
	// build flags, otherwise a target to be built.
	for _, arg := range args {
		if strings.Contains(arg, "=") {
			info.buildFlags = append(info.buildFlags, arg)
		} else {
			info.targets = append(info.targets, normalizeTarget(arg))
		}
	}

	// Create a hash from all sorted build flags and a unique build directory for this set of flags.
	sort.Strings(info.buildFlags)
	buildConfigHash := crc32.ChecksumIEEE([]byte(strings.Join(info.buildFlags, "#")))
	buildConfigName := fmt.Sprintf("%s-%08X", buildDirName, buildConfigHash)
	buildDir := path.Join(workspaceRoot, buildDirName, buildConfigName)
	info.buildOutputDir = path.Join(buildDir, outputDirName)
	info.buildFilesDir = path.Join(buildDir, buildFilesDirName)

	log.Debug("Build flags: '%s'.\n", strings.Join(info.buildFlags, " "))
	log.Debug("Build config: '%s'.\n", buildConfigName)
	log.Debug("Source directory: '%s'.\n", info.sourceDir)
	log.Debug("Build directory: '%s'.\n", buildDir)

	// Remove all existing buildfiles.
	util.RemoveDir(info.buildFilesDir)

	// Copy all BUILD.go files and RULES/ files from the source directory.
	modules := module.GetAllModulePaths(workspaceRoot)
	packages := []string{}
	for modName, modPath := range modules {
		modBuildfilesDir := path.Join(info.buildFilesDir, modName)
		modulePackages := copyBuildAndRuleFiles(modName, modPath, modBuildfilesDir, modules)
		packages = append(packages, modulePackages...)
	}

	createGeneratorMainFile(info.buildFilesDir, packages, modules)
	return info
}

func copyBuildAndRuleFiles(moduleName, modulePath, buildFilesDir string, modules map[string]string) []string {
	packages := []string{}

	log.Debug("Processing module '%s'.\n", moduleName)

	modFileContent := createModFileContent(moduleName, modules, "..")
	util.WriteFile(path.Join(buildFilesDir, modFileName), modFileContent)

	buildFiles := []string{}
	err := util.WalkSymlink(modulePath, func(filePath string, file os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relativeFilePath := strings.TrimPrefix(filePath, modulePath+"/")

		// Ignore the BUILD/, DEPS/ and RULES/ directories.
		if file.IsDir() && (relativeFilePath == buildDirName || relativeFilePath == util.DepsDirName || relativeFilePath == RulesDirName) {
			return filepath.SkipDir
		}

		// Skip everything that is not a BUILD.go file.
		if file.IsDir() || file.Name() != buildFileName {
			return nil
		}

		log.Debug("Found build file '%s'.\n", path.Join(modulePath, relativeFilePath))
		buildFiles = append(buildFiles, filePath)
		return nil
	})

	if err != nil {
		log.Fatal("Failed to search module '%s' for '%s' files: %s.\n", moduleName, buildFileName, err)
	}

	for _, buildFile := range buildFiles {
		relativeFilePath := strings.TrimPrefix(buildFile, modulePath+"/")
		relativeDirPath := strings.TrimSuffix(path.Dir(relativeFilePath), "/")

		packages = append(packages, path.Join(moduleName, relativeDirPath))
		packageName, targets := parseBuildFile(buildFile)
		targetLines := []string{}
		for _, targetName := range targets {
			targetLines = append(targetLines, fmt.Sprintf("    ctx.AddTarget(reflect.TypeOf(__internal_pkg{}).PkgPath()+\"/%s\", %s)", targetName, targetName))
		}

		initFileContent := fmt.Sprintf(initFileTemplate, packageName, strings.Join(targetLines, "\n"))
		initFilePath := path.Join(buildFilesDir, relativeDirPath, initFileName)
		util.WriteFile(initFilePath, []byte(initFileContent))

		copyFilePath := path.Join(buildFilesDir, relativeFilePath)
		util.CopyFile(buildFile, copyFilePath)
	}

	rulesDirPath := path.Join(modulePath, RulesDirName)
	if !util.DirExists(rulesDirPath) {
		log.Debug("Module '%s' does not specify any build rules.\n", moduleName)
		return packages
	}

	err = filepath.Walk(rulesDirPath, func(filePath string, file os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if file.IsDir() || path.Ext(file.Name()) != ".go" {
			return nil
		}

		relativeFilePath := strings.TrimPrefix(filePath, modulePath+"/")
		copyFilePath := path.Join(buildFilesDir, relativeFilePath)
		util.CopyFile(filePath, copyFilePath)
		return nil
	})

	if err != nil {
		log.Fatal("Failed to copy rule files for module '%s': %s.\n", moduleName, err)
	}

	return packages
}

func createModFileContent(moduleName string, modules map[string]string, pathPrefix string) []byte {
	mod := strings.Builder{}

	fmt.Fprintf(&mod, "module %s\n\n", moduleName)
	fmt.Fprintf(&mod, "go %s\n\n", goVersion)

	for modName := range modules {
		fmt.Fprintf(&mod, "require %s v0.0.0\n", modName)
		fmt.Fprintf(&mod, "replace %s => %s/%s\n\n", modName, pathPrefix, modName)
	}

	fmt.Fprintf(&mod, "require dbt v0.0.0\n")
	fmt.Fprintf(&mod, "replace dbt => %s\n\n", dbtModulePath)

	return []byte(mod.String())
}

func parseBuildFile(buildFilePath string) (string, []string) {
	fileAst, err := parser.ParseFile(token.NewFileSet(), buildFilePath, nil, parser.AllErrors)

	if err != nil {
		log.Fatal("Failed to parse '%s': %s.\n", buildFilePath, err)
	}

	targets := []string{}

	for _, decl := range fileAst.Decls {
		decl, ok := decl.(*ast.GenDecl)
		if !ok {
			log.Fatal("'%s' contains invalid declarations. Only import statements and 'var' declarations are allowed.\n", buildFilePath)
		}

		for _, spec := range decl.Specs {
			switch spec := spec.(type) {
			case *ast.ImportSpec:
			case *ast.ValueSpec:
				if decl.Tok.String() != "var" {
					log.Fatal("'%s' contains invalid declarations. Only import statements and 'var' declarations are allowed.\n", buildFilePath)
				}
				for _, id := range spec.Names {
					if id.Name == "_" {
						log.Fatal("'%s' contains anonymous target declarations. All targets must have a name.\n", buildFilePath)
					}
					targets = append(targets, id.Name)
				}
			default:
				log.Fatal("'%s' contains invalid declarations. Only import statements and 'var' declarations are allowed.\n", buildFilePath)
			}
		}
	}

	return fileAst.Name.String(), targets
}

func createGeneratorMainFile(buildFilesDir string, packages []string, modules map[string]string) {
	importLines := []string{}
	dbtMainLines := []string{}
	for idx, pkg := range packages {
		importLines = append(importLines, fmt.Sprintf("import p%d \"%s\"", idx, pkg))
		dbtMainLines = append(dbtMainLines, fmt.Sprintf("    p%d.DbtMain(ctx)", idx))
	}

	mainFilePath := path.Join(buildFilesDir, mainFileName)
	mainFileContent := fmt.Sprintf(mainFileTemplate, strings.Join(importLines, "\n"), strings.Join(dbtMainLines, "\n"))
	util.WriteFile(mainFilePath, []byte(mainFileContent))

	modFilePath := path.Join(buildFilesDir, modFileName)
	modFileContent := createModFileContent("root", modules, ".")
	util.WriteFile(modFilePath, modFileContent)
}

func getAvailableTargets(info buildInfo) map[string]struct{} {
	return getAvailable("targets", info)
}

func getAvailableFlags(info buildInfo) map[string]struct{} {
	return getAvailable("flags", info)
}

func getAvailable(kind string, info buildInfo) map[string]struct{} {
	stdout := runGenerator(info, kind)
	lines := strings.Split(string(stdout.Bytes()), "\n")
	result := map[string]struct{}{}
	for _, line := range lines {
		if line != "" {
			result[line] = struct{}{}
		}
	}
	return result
}

func runGenerator(info buildInfo, mode string) bytes.Buffer {
	var stdout, stderr bytes.Buffer
	cmdArgs := append([]string{"run", mainFileName, mode, info.sourceDir, info.buildOutputDir, info.workingDir}, info.buildFlags...)
	cmd := exec.Command("go", cmdArgs...)
	cmd.Dir = info.buildFilesDir
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	err := cmd.Run()
	fmt.Print(string(stderr.Bytes()))
	if err != nil {
		log.Fatal("Failed to run generator in mode '%s': %s.\n", mode, err)
	}
	return stdout
}

func runNinja(info buildInfo) {
	// Produce the ninja.build file.
	ninjaFileContent := runGenerator(info, "ninja")
	ninjaFilePath := path.Join(info.buildOutputDir, ninjaFileName)
	util.WriteFile(ninjaFilePath, ninjaFileContent.Bytes())

	args := info.ninjaTargets
	if log.Verbose {
		args = append([]string{"-v"}, args...)
	}
	cmd := exec.Command("ninja", args...)
	cmd.Dir = info.buildOutputDir
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	err := cmd.Run()
	if err != nil {
		log.Fatal("Ninja failed: %s\n", err)
	}
}

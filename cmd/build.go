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

	"github.com/spf13/cobra"
)

const buildDirName = "BUILD"
const buildFileName = "BUILD.go"
const buildFilesDirName = "buildfiles"
const dbtModulePath = "github.com/daedaleanai/dbt v0.1.1"
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

type __internal_buildable interface {
	BuildSteps() []core.BuildStep
}

func init() {
    steps := []core.BuildStep{}

%s

	for _, step := range steps {
		step.Print()
	}
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

%s

func main() {}
`

var buildCmd = &cobra.Command{
	Use:   "build <targets> -- <build flags>",
	Args:  cobra.MinimumNArgs(1),
	Short: "Builds the targets",
	Long:  `Builds the targets.`,
	Run:   runBuild,
}

func init() {
	rootCmd.AddCommand(buildCmd)
}

func runBuild(cmd *cobra.Command, args []string) {
	workspaceRoot := util.GetWorkspaceRoot()
	sourceDir := path.Join(workspaceRoot, util.DepsDirName)

	// Split all args into two categories: If they start with "--" they are considered
	// a build flag, otherwise a target to be built.
	targets := []string{}
	buildFlags := []string{}

	for _, arg := range args {
		if strings.HasPrefix(arg, "--") {
			buildFlags = append(buildFlags, arg)
			continue
		}
		// Build targets are interpreted as relative to the workspace root when they start with a '//'.
		// Otherwise they are interpreted as relative to the current working directory.
		// E.g.: Running 'dbt build //src/path/to/mylib.a' from anywhere in the workspace is equivallent
		// to running 'dbt build mylib.a' in '.../src/path/to/' or running 'dbt build path/to/mylib.a' in '.../src/'.
		if !strings.HasPrefix(arg, string(os.PathSeparator)+string(os.PathSeparator)) {
			workingDir, _ := os.Getwd()
			arg = path.Join(workingDir, arg)
			moduleRoot := util.GetModuleRootForPath(arg)
			arg = strings.TrimPrefix(arg, path.Dir(moduleRoot))
		}
		targets = append(targets, strings.TrimLeft(arg, string(os.PathSeparator)))
	}

	// Create a hash from all sorted build flags and a unique build directory for this set of flags.
	sort.Strings(buildFlags)
	buildConfigHash := crc32.ChecksumIEEE([]byte(strings.Join(buildFlags, "#")))
	buildConfigName := fmt.Sprintf("%s-%08X", buildDirName, buildConfigHash)
	buildDir := path.Join(workspaceRoot, buildDirName, buildConfigName, outputDirName)

	log.Debug("Building targets '%s'.\n", strings.Join(targets, "', '"))
	log.Debug("Build flags: '%s'.\n", strings.Join(buildFlags, " "))
	log.Debug("Build config: '%s'.\n", buildConfigName)
	log.Debug("Source directory: '%s'.\n", sourceDir)
	log.Debug("Build directory: '%s'.\n", buildDir)

	// Remove all existing buildfiles.
	buildFilesDir := path.Join(workspaceRoot, buildDirName, buildConfigName, buildFilesDirName)
	util.RemoveDir(buildFilesDir)

	// Copy all BUILD.go files and RULES/ files from the source directory.
	modules := module.GetAllModulePaths(workspaceRoot)
	importLines := []string{}
	for modName, modPath := range modules {
		modBuildfilesDir := path.Join(buildFilesDir, modName)
		moduleImportLines := copyBuildAndRuleFiles(modName, modPath, modBuildfilesDir, modules)
		importLines = append(importLines, moduleImportLines...)
	}

	// Compile all build files and run the resulting binary.
	// This will produce the build.ninja file.
	generateNinjaFile(sourceDir, buildDir, buildFilesDir, importLines, buildFlags, modules)

	// Call Ninja to build the targets.
	runNinja(buildDir, targets)
}

func copyBuildAndRuleFiles(moduleName, modulePath, buildFilesDir string, modules map[string]string) []string {
	importLines := []string{}

	log.Debug("Processing module '%s'.\n", moduleName)

	modFileContent := createModFileContent(moduleName, modules, "..")
	util.WriteFile(path.Join(buildFilesDir, modFileName), modFileContent)

	buildFiles := []string{}
	err := util.WalkSymlink(modulePath, func(filePath string, file os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relativeFilePath := strings.TrimPrefix(filePath, modulePath+string(os.PathSeparator))

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
		relativeFilePath := strings.TrimPrefix(buildFile, modulePath+string(os.PathSeparator))
		relativeDirPath := strings.TrimSuffix(path.Dir(relativeFilePath), string(os.PathSeparator))

		importLine := fmt.Sprintf("import _ \"%s/%s\"", moduleName, relativeDirPath)
		if relativeDirPath == "." {
			importLine = fmt.Sprintf("import _ \"%s\"", moduleName)
		}
		importLines = append(importLines, importLine)

		packageName, targets := parseBuildFile(buildFile)
		targetLines := []string{}
		for _, targetName := range targets {
			targetLine := fmt.Sprintf(`
			if iface, ok := interface{}(%s).(__internal_buildable); ok {
				core.CurrentTarget = reflect.TypeOf(__internal_pkg{}).PkgPath()+"/%s"
				steps = append(steps, iface.BuildSteps()...)
			}`, targetName, targetName)
			targetLines = append(targetLines, targetLine)
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
		return importLines
	}

	err = filepath.Walk(rulesDirPath, func(filePath string, file os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if file.IsDir() || path.Ext(file.Name()) != ".go" {
			return nil
		}

		relativeFilePath := strings.TrimPrefix(filePath, modulePath+string(os.PathSeparator))
		copyFilePath := path.Join(buildFilesDir, relativeFilePath)
		util.CopyFile(filePath, copyFilePath)
		return nil
	})

	if err != nil {
		log.Fatal("Failed to copy rule files for module '%s': %s.\n", moduleName, err)
	}

	return importLines
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

func generateNinjaFile(sourceDir, buildDir, buildFilesDir string, importLines []string, buildFlags []string, modules map[string]string) {
	mainFilePath := path.Join(buildFilesDir, mainFileName)
	mainFileContent := fmt.Sprintf(mainFileTemplate, strings.Join(importLines, "\n"))
	util.WriteFile(mainFilePath, []byte(mainFileContent))

	modFilePath := path.Join(buildFilesDir, modFileName)
	modFileContent := createModFileContent("root", modules, ".")
	util.WriteFile(modFilePath, modFileContent)

	var stdout, stderr bytes.Buffer
	args := append([]string{"run", mainFileName, sourceDir, buildDir}, buildFlags...)
	cmd := exec.Command("go", args...)
	cmd.Dir = buildFilesDir
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	err := cmd.Run()
	fmt.Println(string(stderr.Bytes()))
	if err != nil {
		log.Fatal("Failed to generate ninja file.\n")
	}

	ninjaFilePath := path.Join(buildDir, ninjaFileName)
	util.WriteFile(ninjaFilePath, stdout.Bytes())
}

func runNinja(buildDir string, targets []string) {
	if log.Verbose {
		targets = append([]string{"-v"}, targets...)
	}
	cmd := exec.Command("ninja", targets...)
	cmd.Dir = buildDir
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	err := cmd.Run()
	if err != nil {
		log.Fatal("Build failed.\n")
	}
}

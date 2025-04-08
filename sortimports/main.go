/*
 * Copyright 2025 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

type Args struct {
	Paths       []string
	ExcludeDirs sets.Set[string]
	SortGroups  []SortGroup
	Verbose     bool
}

// SortGroup is a group that collect a list imports based on specific Prefix
// Index used to mark the place of the group in the order
type SortGroup struct {
	Index  int
	Prefix string
}

type Libs struct {
	sortGroup  SortGroup
	importSpec []*ast.ImportSpec
}

// Imports sorting is based on the configuration provided by the sortfile.
// The default is:
// 1. Standard libraries
// 2. "k8s.io" imports
// 3. "sigs.k8s.io" imports
// 4. Other third-party libraries
// 5. "github.com/openshift" imports
// 6. "github.com/openshift-kni" imports
//
// Custom sorting function for import specs
func sortImports(imports []*ast.ImportSpec, sortGroups []SortGroup) []*ast.ImportSpec {
	libsMap := makeLibsMap(sortGroups)

	sortImport(libsMap, imports, 0)

	for _, libs := range libsMap {
		// Sort each group while keeping original comments and aliases
		sort.Slice(libs.importSpec, func(i, j int) bool { return libs.importSpec[i].Path.Value < libs.importSpec[j].Path.Value })
	}

	// sort by index
	var specs = make([][]*ast.ImportSpec, len(libsMap))
	for _, libs := range libsMap {
		specs[libs.sortGroup.Index-1] = libs.importSpec
	}

	// Reconstruct sorted import block
	var sortedImports []*ast.ImportSpec
	for _, spec := range specs {
		if len(spec) > 0 {
			sortedImports = append(sortedImports, spec...)
			sortedImports = append(sortedImports, newLine())
		}
	}
	return sortedImports
}

func newLine() *ast.ImportSpec {
	return &ast.ImportSpec{
		Path:    &ast.BasicLit{Kind: token.STRING, Value: ``},
		Comment: &ast.CommentGroup{List: []*ast.Comment{{Text: "// blank line"}}},
	}
}

func sortImport(libsMap map[string]*Libs, imports []*ast.ImportSpec, i int) {
	if i >= len(imports) {
		return
	}
	path := strings.Trim(imports[i].Path.Value, `"`)
	for prefix, libs := range libsMap {
		if strings.HasPrefix(path, prefix) {
			libs.importSpec = append(libs.importSpec, imports[i])
			sortImport(libsMap, imports, i+1)
			return
		}
	}

	if _, ok := libsMap["std"]; ok && isStdLib(path) {
		libsMap["std"].importSpec = append(libsMap["std"].importSpec, imports[i])
	} else {
		libsMap["default"].importSpec = append(libsMap["default"].importSpec, imports[i])
	}

	sortImport(libsMap, imports, i+1)
}

func makeLibsMap(groups []SortGroup) map[string]*Libs {
	libs := make(map[string]*Libs, len(groups))
	for _, group := range groups {
		libs[group.Prefix] = &Libs{
			sortGroup:  group,
			importSpec: []*ast.ImportSpec{},
		}
	}
	// add default if not exist
	if _, ok := libs["default"]; !ok {
		libs["default"] = &Libs{
			sortGroup: SortGroup{
				Index: len(groups) + 1,
			},
			importSpec: []*ast.ImportSpec{},
		}
	}
	return libs
}

// Determines if a package is a standard library package
func isStdLib(pkg string) bool {
	// Assume anything without a dot (.) is a std lib package
	return !strings.Contains(pkg, ".")
}

// Process the file while preserving alias & comments in imports
func processFile(src []byte, groups []SortGroup) ([]byte, error) {
	// Parse the file
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("failed to parse file: %w", err)
	}

	// Extract imports
	var imports []*ast.ImportSpec
	var importDecl *ast.GenDecl

	// Find import declaration
	for _, decl := range node.Decls {
		if genDecl, ok := decl.(*ast.GenDecl); ok && genDecl.Tok == token.IMPORT {
			importDecl = genDecl
			for _, spec := range genDecl.Specs {
				imports = append(imports, spec.(*ast.ImportSpec))
			}
		}
	}

	// do nothing if no imports are found
	if importDecl == nil {
		return src, nil
	}

	// Sort imports
	sortedImports := sortImports(imports, groups)

	// Replace original imports with sorted ones
	importDecl.Specs = nil
	for _, imp := range sortedImports {
		importDecl.Specs = append(importDecl.Specs, imp)
	}

	// Format & write back the file
	var formattedCode bytes.Buffer
	err = format.Node(&formattedCode, fset, node)
	if err != nil {
		return nil, fmt.Errorf("failed to format code: %v", err)
	}
	return formattedCode.Bytes(), nil
}

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("Usage: %s <path-to-files>\n", os.Args[0])
	}
	progArgs, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	for _, rootPath := range progArgs.Paths {
		err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
			if d.IsDir() {
				if progArgs.ExcludeDirs.Has(path) {
					return filepath.SkipDir
				}
				// we'll return to the files in this dir later
				return nil
			}
			// skip non Go files
			if !strings.HasSuffix(path, ".go") {
				return nil
			}

			if progArgs.Verbose {
				fmt.Printf("sorting imports for file: %q\n", path)
			}

			src, err2 := os.ReadFile(path)
			if err2 != nil {
				return fmt.Errorf("failed to read file: %w", err2)
			}

			sortedSrc, err2 := processFile(src, progArgs.SortGroups)
			if err2 != nil {
				return fmt.Errorf("failed to sort imports: %w", err2)
			}

			if err2 := os.WriteFile(path, sortedSrc, 0644); err2 != nil {
				return fmt.Errorf("failed to write file: %w", err2)
			}
			return nil
		})
		if err != nil {
			log.Fatal(err)
		}
	}
}

func parseFlags(args []string) (*Args, error) {
	progArgs := &Args{
		Paths:       make([]string, 0),
		ExcludeDirs: make(sets.Set[string]),
	}
	paths := flag.String("paths", "", "Path to files to be sorted, multiple paths can be provided.")
	excludeDirs := flag.String("exclude-dirs", "", "Directory names that should be excluded from imports, multiple directories can be provided.")
	path := flag.String("sort-file-path", "sortfile.txt", "A path to file of list of prefixes and indexes on which the tool will based its sorting")
	flag.BoolVar(&progArgs.Verbose, "verbose", false, "Verbose output")
	flag.Parse()

	if err := prepPaths(*paths, progArgs); err != nil {
		return nil, err
	}

	if err := prepExcludeDirs(*excludeDirs, progArgs); err != nil {
		return nil, err
	}

	if err := prepSortGroups(*path, progArgs); err != nil {
		return nil, err
	}

	return progArgs, nil
}

func prepSortGroups(path string, progArgs *Args) error {
	file, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	progArgs.SortGroups, err = parseSortFile(file)
	if err != nil {
		return err
	}
	return nil
}

func parseSortFile(data []byte) ([]SortGroup, error) {
	var err error
	var groups []SortGroup
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		group := SortGroup{}
		// skip for comments
		if strings.HasPrefix(line, "//") {
			continue
		}

		indexAndPrefix := strings.Split(line, " ")
		// skip on invalid formats
		if len(indexAndPrefix) != 2 {
			continue
		}

		index := indexAndPrefix[0]
		group.Index, err = strconv.Atoi(index)
		if err != nil {
			return nil, fmt.Errorf("%s must be a valid number; %w", indexAndPrefix, err)
		}
		group.Prefix = indexAndPrefix[1]
		groups = append(groups, group)
	}
	return groups, nil
}

func prepPaths(paths string, progArgs *Args) error {
	if paths == "" {
		return fmt.Errorf("no paths specified")
	}
	progArgs.Paths = strings.Split(paths, " ")
	return cleanPaths(progArgs.Paths)
}

func prepExcludeDirs(excludeDirs string, progArgs *Args) error {
	if excludeDirs == "" {
		return nil
	}
	dirs := strings.Split(excludeDirs, " ")
	if err := cleanPaths(dirs); err != nil {
		return err
	}
	for _, dir := range dirs {
		progArgs.ExcludeDirs.Insert(dir)
	}
	return nil
}

func cleanPaths(paths []string) error {
	var errs []error
	for i, path := range paths {
		cleanPath := filepath.Clean(path)
		if cleanPath == "" {
			errs = append(errs, fmt.Errorf("path %q is invalid", path))
			continue
		}
		paths[i] = cleanPath
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

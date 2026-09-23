package parser

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	stageTagRegex    = regexp.MustCompile(`(?i)(?:Stage|Spec):\s*(?:\[Stage:\s*)?([^\]\r\n]+)\]?`)
	scenarioTagRegex = regexp.MustCompile(`(?i)(?:Scenario|Requirement):\s*(.+)`)
)

// ParseTestDir inspects a directory for Go test files (*_test.go) and extracts scenarios.
// If the directory does not exist or contains no test files, an empty TestSuite is returned without error.
func ParseTestDir(dir string) (*TestSuite, error) {
	suite := &TestSuite{
		Directory: dir,
		Files:     []*TestFile{},
		Scenarios: []*TestScenario{},
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return suite, nil
		}
		return nil, fmt.Errorf("reading test directory %q: %w", dir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		testFile, err := ParseTestFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("parsing test file %q: %w", filePath, err)
		}

		suite.Files = append(suite.Files, testFile)
		suite.Scenarios = append(suite.Scenarios, testFile.Scenarios...)
	}

	return suite, nil
}

// ParseTestFile inspects a single Go test file and extracts top-level Test* functions and t.Run subtests.
func ParseTestFile(path string) (*TestFile, error) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing Go file %q: %w", path, err)
	}

	testFile := &TestFile{
		FilePath:  path,
		Package:   node.Name.Name,
		Scenarios: []*TestScenario{},
	}

	for _, decl := range node.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		if !strings.HasPrefix(fn.Name.Name, "Test") {
			continue
		}

		docText := ""
		if fn.Doc != nil {
			docText = strings.TrimSpace(fn.Doc.Text())
		}

		targetStage, targetReq := extractScenarioTags(docText)
		startPos := fset.Position(fn.Pos())

		// Look for t.Run subtests in the function body
		subtests := findSubtests(fn.Body)

		if len(subtests) == 0 {
			// Standalone test function
			scenario := &TestScenario{
				Name:              fn.Name.Name,
				Package:           testFile.Package,
				FilePath:          path,
				LineNumber:        startPos.Line,
				Doc:               docText,
				TargetStage:       targetStage,
				TargetRequirement: targetReq,
			}
			testFile.Scenarios = append(testFile.Scenarios, scenario)
		} else {
			// Function with t.Run subtests
			for _, st := range subtests {
				subScenario := &TestScenario{
					Name:              fmt.Sprintf("%s/%s", fn.Name.Name, st.Name),
					Package:           testFile.Package,
					FilePath:          path,
					LineNumber:        fset.Position(st.Pos).Line,
					Doc:               docText,
					TargetStage:       targetStage,
					TargetRequirement: targetReq,
				}
				testFile.Scenarios = append(testFile.Scenarios, subScenario)
			}
		}
	}

	return testFile, nil
}

type subtestInfo struct {
	Name string
	Pos  token.Pos
}

// findSubtests scans a function body for t.Run("subtest_name", ...) calls.
func findSubtests(body *ast.BlockStmt) []subtestInfo {
	if body == nil {
		return nil
	}

	var subtests []subtestInfo

	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Run" {
			return true
		}

		if len(call.Args) >= 1 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				val, err := strconv.Unquote(lit.Value)
				if err == nil {
					subtests = append(subtests, subtestInfo{
						Name: val,
						Pos:  call.Pos(),
					})
				}
			}
		}

		return true
	})

	return subtests
}

// extractScenarioTags parses metadata annotations from doc comments.
func extractScenarioTags(doc string) (stage string, requirement string) {
	if doc == "" {
		return "", ""
	}

	if matches := stageTagRegex.FindStringSubmatch(doc); matches != nil {
		stage = strings.TrimSpace(matches[1])
	}
	if matches := scenarioTagRegex.FindStringSubmatch(doc); matches != nil {
		requirement = strings.TrimSpace(matches[1])
	}

	return stage, requirement
}

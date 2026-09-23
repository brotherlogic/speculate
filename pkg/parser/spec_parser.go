package parser

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	titleRegex      = regexp.MustCompile(`^#\s+(.+)$`)
	stageRegex      = regexp.MustCompile(`(?i)^##\s*\[Stage:\s*([^\]]+)\]\s*(.*)$`)
	dependsOnRegex  = regexp.MustCompile(`(?i)[\(\[]depends\s*on:\s*([^\)\]]+)[\)\]]`)
	bulletItemRegex = regexp.MustCompile(`^\s*(?:[-*]|\d+\.)\s+(.+)$`)

	// RPC patterns:
	// Matches `Put(key, value)`: or Put(key, value): or `Put`: or Put:
	rpcMethodPrefixRegex = regexp.MustCompile(`^` + "`?" + `([A-Za-z0-9_]+)(?:\([^\)]*\))?` + "`?" + `:\s*(.*)$`)
	// Matches "If `Get` is invoked..." or "If Get is invoked..."
	rpcIfInvokedRegex = regexp.MustCompile(`(?i)^If\s+` + "`?" + `([A-Za-z0-9_]+)` + "`?" + `\s+is\s+invoked`)
)

// ParseSpec parses a staged markdown specification from a raw string.
func ParseSpec(content string) (*Specification, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	spec := &Specification{
		Stages: []*Stage{},
	}

	var currentStage *Stage
	reqCountInStage := 0
	lineNumber := 0

	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			continue
		}

		// 1. Check for specification title (# Title)
		if spec.Title == "" {
			if matches := titleRegex.FindStringSubmatch(trimmed); matches != nil {
				spec.Title = strings.TrimSpace(matches[1])
				continue
			}
		}

		// 2. Check for stage header (## [Stage: Name] Description)
		if matches := stageRegex.FindStringSubmatch(trimmed); matches != nil {
			stageName := strings.TrimSpace(matches[1])
			remainingHeader := strings.TrimSpace(matches[2])

			var dependsOn []string
			description := remainingHeader

			if depMatches := dependsOnRegex.FindStringSubmatch(remainingHeader); depMatches != nil {
				depsRaw := depMatches[1]
				for _, dep := range strings.Split(depsRaw, ",") {
					d := strings.TrimSpace(dep)
					if d != "" {
						dependsOn = append(dependsOn, d)
					}
				}
				// Remove the dependency clause from the description
				description = strings.TrimSpace(dependsOnRegex.ReplaceAllString(remainingHeader, ""))
			}

			currentStage = &Stage{
				Name:         stageName,
				RawHeader:    trimmed,
				Description:  description,
				DependsOn:    dependsOn,
				Requirements: []*Requirement{},
				LineNumber:   lineNumber,
			}
			spec.Stages = append(spec.Stages, currentStage)
			reqCountInStage = 0
			continue
		}

		// 3. Check for requirement bullet item within a stage
		if currentStage != nil {
			if matches := bulletItemRegex.FindStringSubmatch(trimmed); matches != nil {
				reqCountInStage++
				rawItem := matches[1]
				targetRPC := extractTargetRPC(rawItem)

				req := &Requirement{
					ID:          fmt.Sprintf("%s-%d", currentStage.Name, reqCountInStage),
					RawText:     rawItem,
					TargetRPC:   targetRPC,
					Description: rawItem,
					LineNumber:  lineNumber,
				}
				currentStage.Requirements = append(currentStage.Requirements, req)
				continue
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading spec: %w", err)
	}

	return spec, nil
}

// ParseSpecFile reads and parses a staged specification markdown file from disk.
func ParseSpecFile(path string) (*Specification, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading spec file %q: %w", path, err)
	}

	spec, err := ParseSpec(string(data))
	if err != nil {
		return nil, err
	}
	spec.FilePath = path
	return spec, nil
}

// extractTargetRPC tries to identify the RPC endpoint being referred to in the requirement text.
func extractTargetRPC(text string) string {
	if matches := rpcMethodPrefixRegex.FindStringSubmatch(text); matches != nil {
		return matches[1]
	}
	if matches := rpcIfInvokedRegex.FindStringSubmatch(text); matches != nil {
		return matches[1]
	}
	return ""
}

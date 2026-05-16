// /thearray/gogents/activities/diff_analysis.go - Enhanced diff analysis functions
package activities

import (
	"bufio"
	"path/filepath"
	"regexp"
	"strings"
)

// scannerBufferStart is the starting capacity for bufio.Scanner buffers, and
// scannerBufferMax is the upper bound. 10MB matches the diff size limit
// applied at fetch time, so any diff small enough to enter this package can
// be scanned line by line.
const (
	scannerBufferStart = 64 * 1024
	scannerBufferMax   = 10 * 1024 * 1024
)

// newDiffScanner constructs a bufio.Scanner sized for the maximum diff we
// will accept, so very long lines do not cause silent truncation.
func newDiffScanner(diff string) *bufio.Scanner {
	scanner := bufio.NewScanner(strings.NewReader(diff))
	scanner.Buffer(make([]byte, 0, scannerBufferStart), scannerBufferMax)
	return scanner
}

// Pre-compiled regexes for function extraction (Issue 18: avoid recompiling per call)
var (
	goFuncRegex      = regexp.MustCompile(`func\s+(?:\([^)]*\)\s+)?(\w+)\s*\(`)
	cFuncRegex       = regexp.MustCompile(`^\s*[\w\s\*]+\s+(\w+)\s*\([^)]*\)\s*\{`)
	jsFuncRegex      = regexp.MustCompile(`function\s+(\w+)\s*\(`)
	jsArrowFuncRegex = regexp.MustCompile(`const\s+(\w+)\s*=\s*\([^)]*\)\s*=>`)
	pyFuncRegex      = regexp.MustCompile(`def\s+(\w+)\s*\(`)
)

// DiffAnalysis contains enhanced analysis results for a PR diff
type DiffAnalysis struct {
	MentionedFunctions []string `json:"mentioned_functions"`
	MentionedFiles     []string `json:"mentioned_files"`
	AddedLines         int      `json:"added_lines"`
	RemovedLines       int      `json:"removed_lines"`
}

// ExtractMentionedFunctions extracts function names from diff changes
func ExtractMentionedFunctions(diff string) []string {
	var functions []string

	scanner := newDiffScanner(diff)
	for scanner.Scan() {
		line := scanner.Text()
		// Skip diff header lines
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			continue
		}
		// Only analyze added or removed lines
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			// Remove the diff prefix
			cleanLine := strings.TrimPrefix(strings.TrimPrefix(line, "+"), "-")

			for _, re := range []*regexp.Regexp{goFuncRegex, cFuncRegex, jsFuncRegex, jsArrowFuncRegex, pyFuncRegex} {
				if matches := re.FindStringSubmatch(cleanLine); len(matches) > 1 {
					functions = append(functions, matches[1])
				}
			}
		}
	}

	return removeDuplicates(functions)
}

// ExtractMentionedFiles extracts file paths from diff headers
func ExtractMentionedFiles(diff string) []string {
	var files []string

	scanner := newDiffScanner(diff)
	for scanner.Scan() {
		line := scanner.Text()
		// Use +++ headers as the canonical source for file paths
		if strings.HasPrefix(line, "+++") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				path := parts[1]
				if strings.HasPrefix(path, "b/") {
					path = path[2:]
				}
				if path != "/dev/null" {
					files = append(files, path)
				}
			}
		}
	}

	return removeDuplicates(files)
}

// AnalyzeDiffMetrics provides comprehensive diff analysis
func AnalyzeDiffMetrics(diff string) DiffAnalysis {
	functions := ExtractMentionedFunctions(diff)
	files := ExtractMentionedFiles(diff)

	addedLines := 0
	removedLines := 0

	scanner := newDiffScanner(diff)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			addedLines++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			removedLines++
		}
	}

	return DiffAnalysis{
		MentionedFunctions: functions,
		MentionedFiles:     files,
		AddedLines:         addedLines,
		RemovedLines:       removedLines,
	}
}

// languageMap maps file extensions to programming languages
var languageMap = map[string]string{
	".go":    "Go",
	".c":     "C",
	".cpp":   "C++",
	".cc":    "C++",
	".cxx":   "C++",
	".h":     "C/C++",
	".hpp":   "C++",
	".js":    "JavaScript",
	".ts":    "TypeScript",
	".jsx":   "React",
	".tsx":   "React/TypeScript",
	".py":    "Python",
	".java":  "Java",
	".rs":    "Rust",
	".rb":    "Ruby",
	".php":   "PHP",
	".cs":    "C#",
	".swift": "Swift",
	".kt":    "Kotlin",
	".scala": "Scala",
	".r":     "R",
	".m":     "Objective-C",
	".mm":    "Objective-C++",
}

// CategorizeFilesByLanguage groups files by their programming language
func CategorizeFilesByLanguage(files []string) map[string][]string {
	categories := make(map[string][]string)

	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		lang := "Unknown"
		if l, ok := languageMap[ext]; ok {
			lang = l
		}
		categories[lang] = append(categories[lang], file)
	}

	return categories
}

// removeDuplicates removes duplicate strings from a slice
func removeDuplicates(slice []string) []string {
	seen := make(map[string]bool)
	var result []string

	for _, item := range slice {
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}

	return result
}

// testBasenamePrefixes are case-insensitive prefixes of the file's BASENAME
// that mark it as a test file. We compare against filepath.Base so substrings
// like "test_" inside `latest_release.go` no longer false-match.
var testBasenamePrefixes = []string{"test_"}

// testBasenameSuffixes are case-insensitive suffixes of the file's BASENAME
// that mark it as a test file.
var testBasenameSuffixes = []string{
	"_test.go",   // Go tests
	"_test.py",   // Python tests
	".test.js",   // JavaScript tests
	".test.ts",   // TypeScript tests
	".test.jsx",  // React tests
	".test.tsx",  // React/TS tests
	".spec.js",   // JavaScript specs
	".spec.ts",   // TypeScript specs
	".spec.jsx",  // React specs
	".spec.tsx",  // React/TS specs
}

// testPathSegments are case-insensitive directory components that indicate
// the path lives under a test tree. We match against `/segment/` boundaries
// so "/test/" inside "fastest/foo.go" cannot trigger a false positive.
var testPathSegments = []string{"/test/", "/tests/", "/__tests__/", "/__test__/"}

// IsTestFile determines if a file path represents a test file. Matching is
// anchored to either the basename (prefix/suffix) or a path-component
// boundary, never to arbitrary substrings of the path.
func IsTestFile(path string) bool {
	lowerPath := strings.ToLower(filepath.ToSlash(path))
	base := strings.ToLower(filepath.Base(lowerPath))

	for _, prefix := range testBasenamePrefixes {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	for _, suffix := range testBasenameSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	// Use slash boundaries on either side so "/test/" only matches a real
	// path component. We pad the path with a leading slash so a path that
	// starts with "test/" still matches "/test/".
	padded := "/" + lowerPath
	for _, seg := range testPathSegments {
		if strings.Contains(padded, seg) {
			return true
		}
	}
	return false
}

// criticalBasenames is the set of file basenames that are themselves critical
// regardless of where they live in the tree. Comparison is case-insensitive
// against filepath.Base only, so `configtest_helper.go` no longer matches
// "config".
var criticalBasenames = map[string]bool{
	"main.go":            true,
	"main.py":            true,
	"index.js":           true,
	"index.ts":           true,
	"app.py":             true,
	"server.go":          true,
	"dockerfile":         true,
	"docker-compose.yml": true,
	"docker-compose.yaml": true,
	"makefile":           true,
	".env":               true,
	"requirements.txt":   true,
	"go.mod":             true,
	"go.sum":             true,
	"package.json":       true,
	"package-lock.json":  true,
	"yarn.lock":          true,
	"pnpm-lock.yaml":     true,
	"cargo.toml":         true,
	"cargo.lock":         true,
}

// criticalBasenamePrefixes are case-insensitive basename prefixes (without
// extension) that mark the file as a config-shaped critical file.
// e.g. `config.yaml`, `config.json`, `config.go`. Note the trailing dot —
// without it, `configtest.go` would still match.
var criticalBasenamePrefixes = []string{"config."}

// IsCriticalFile determines if a file is critical for system operation.
// Matching is anchored to the basename so non-critical files containing one
// of these substrings (e.g. `configtest_helper.go`) are not flagged.
func IsCriticalFile(path string) bool {
	base := strings.ToLower(filepath.Base(filepath.ToSlash(path)))
	if criticalBasenames[base] {
		return true
	}
	for _, prefix := range criticalBasenamePrefixes {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	// Bare "config" basename (no extension) is also critical.
	if base == "config" {
		return true
	}
	return false
}

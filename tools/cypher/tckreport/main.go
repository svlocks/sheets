// Command tckreport expands and evaluates the checksum-pinned openCypher M23
// TCK against sheets's syntax frontend and the representable semantic subset.
// It emits evidence, not a claim of openCypher certification.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const tckSHA256 = "6deb4acffb301c926cb0811e11b2422704cad2e48fc0a42e40c401a7ee1fba49"

var namedGraphSHA256 = map[string]string{
	"binary-tree-1": "fbcada6966edb9e2d66b1a11a4f8a4c906a9da6afd622640ab08c686962d42da",
	"binary-tree-2": "923bcaf5686ea9051f46ad5c440a286381fd111daad5dbd6377aa3edd7dbfc4c",
}

const (
	statusPass               = "pass"
	statusTypedUnsupported   = "typed_unsupported"
	statusParseRejected      = "parse_rejected"
	statusSemanticFailure    = "semantic_failure"
	statusHarnessUnsupported = "harness_unsupported"

	frontendBound            = "bound"
	frontendTypedUnsupported = "typed_unsupported"
	frontendParseRejected    = "parse_rejected"
)

type scenarioResult struct {
	ID             string   `json:"id"`
	DefinitionID   string   `json:"definition_id,omitempty"`
	Example        int      `json:"example,omitempty"`
	Status         string   `json:"status"`
	FrontendStatus string   `json:"frontend_status"`
	Query          string   `json:"query,omitempty"`
	Queries        []string `json:"queries,omitempty"`
	Stage          string   `json:"stage,omitempty"`
	ExpectedPhase  string   `json:"expected_phase,omitempty"`
	ErrorPhase     string   `json:"error_phase,omitempty"`
	Expected       string   `json:"expected,omitempty"`
	Actual         string   `json:"actual,omitempty"`
	Error          string   `json:"error,omitempty"`
}

type report struct {
	Release             string            `json:"release"`
	ArchiveSHA256       string            `json:"archive_sha256"`
	FixtureSHA256       map[string]string `json:"fixture_sha256"`
	SemanticExecution   bool              `json:"semantic_execution"`
	FeatureFiles        int               `json:"feature_files"`
	ScenarioDefinitions int               `json:"scenario_definitions"`
	ScenarioOutlines    int               `json:"scenario_outlines"`
	ScenarioInstances   int               `json:"scenario_instances"`
	Bound               int               `json:"bound"`
	TypedUnsupported    int               `json:"typed_unsupported"`
	ParseRejected       int               `json:"parse_rejected"`
	Passed              int               `json:"passed"`
	SemanticFailures    int               `json:"semantic_failures"`
	HarnessUnsupported  int               `json:"harness_unsupported"`
	SilentSkips         int               `json:"silent_skips"`
	Limitations         []string          `json:"limitations"`
	Scenarios           []scenarioResult  `json:"scenarios"`
}

type capabilityManifest struct {
	Release       string           `json:"release"`
	ArchiveSHA256 string           `json:"archive_sha256"`
	Cases         []capabilityCase `json:"cases"`
}

type capabilityCase struct {
	ID       string `json:"id"`
	Expected string `json:"expected"`
	Test     string `json:"test"`
}

func main() {
	archivePath := flag.String("archive", ".cache/opencypher/M23/tck-M23.zip", "path to the pinned TCK zip")
	fixtureDirectory := flag.String("fixtures", ".cache/opencypher/M23/graphs", "directory containing checksum-pinned named graph fixtures")
	manifestPath := flag.String("manifest", "tools/cypher/capabilities.json", "capability manifest to verify")
	semantic := flag.Bool("semantic", true, "execute the representable semantic subset in fresh temporary stores")
	flag.Parse()

	result, err := buildReport(*archivePath, *fixtureDirectory, *semantic)
	if err != nil {
		fatal(err)
	}
	if *manifestPath != "" {
		if err := verifyManifest(*manifestPath, result); err != nil {
			fatal(err)
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fatal(err)
	}
}

func buildReport(archivePath, fixtureDirectory string, semantic bool) (result *report, returnErr error) {
	digest, err := fileSHA256(archivePath)
	if err != nil {
		return nil, err
	}
	if digest != tckSHA256 {
		return nil, fmt.Errorf("TCK checksum mismatch: expected %s, got %s", tckSHA256, digest)
	}
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := archive.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	namedGraphs, fixtureDigests, err := loadNamedGraphFixtures(fixtureDirectory)
	if err != nil {
		return nil, err
	}

	result = &report{
		Release:           "openCypher 9 M23",
		ArchiveSHA256:     digest,
		FixtureSHA256:     fixtureDigests,
		SemanticExecution: semantic,
		SilentSkips:       0,
		Limitations: []string{
			"sheets is not certified as openCypher-compatible",
			"sheets has no temporary custom-procedure catalog for TCK procedure fixtures",
			"error-code comparison is limited to categories with stable sheets error evidence",
		},
	}
	for _, file := range archive.File {
		if !strings.HasPrefix(file.Name, "tck/features/") || !strings.HasSuffix(file.Name, ".feature") {
			continue
		}
		result.FeatureFiles++
		document, err := readFeature(file)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file.Name, err)
		}
		result.ScenarioDefinitions += len(document.Definitions)
		for _, definition := range document.Definitions {
			if definition.Outline {
				result.ScenarioOutlines++
			}
		}
		for _, instance := range document.Instances {
			result.Scenarios = append(result.Scenarios, runScenario(instance, namedGraphs, semantic))
		}
	}
	sort.Slice(result.Scenarios, func(i, j int) bool { return result.Scenarios[i].ID < result.Scenarios[j].ID })
	result.ScenarioInstances = len(result.Scenarios)
	if err := summarizeScenarios(result); err != nil {
		return nil, err
	}
	return result, nil
}

func summarizeScenarios(result *report) error {
	for _, scenario := range result.Scenarios {
		switch scenario.FrontendStatus {
		case frontendBound:
			result.Bound++
		case frontendTypedUnsupported:
			result.TypedUnsupported++
		case frontendParseRejected:
			result.ParseRejected++
		}
		switch scenario.Status {
		case statusPass:
			result.Passed++
		case statusSemanticFailure:
			result.SemanticFailures++
		case statusHarnessUnsupported:
			result.HarnessUnsupported++
		case statusTypedUnsupported, statusParseRejected:
			// These terminal frontend classifications are counted above. They
			// never enter semantic execution.
		default:
			return fmt.Errorf("scenario %s has unknown status %q", scenario.ID, scenario.Status)
		}
	}
	classified := result.Passed + result.SemanticFailures + result.HarnessUnsupported + result.TypedUnsupported + result.ParseRejected
	result.SilentSkips = result.ScenarioInstances - classified
	if result.SilentSkips != 0 {
		return fmt.Errorf("TCK report left %d scenario instances unclassified", result.SilentSkips)
	}
	return nil
}

func loadNamedGraphFixtures(directory string) (map[string]string, map[string]string, error) {
	fixtures := make(map[string]string, len(namedGraphSHA256))
	digests := make(map[string]string, len(namedGraphSHA256))
	for name, expected := range namedGraphSHA256 {
		filename := directory + string(os.PathSeparator) + name + ".cypher"
		actual, err := fileSHA256(filename)
		if err != nil {
			return nil, nil, fmt.Errorf("named graph fixture %s: %w", name, err)
		}
		if actual != expected {
			return nil, nil, fmt.Errorf("named graph fixture %s checksum mismatch: expected %s, got %s", name, expected, actual)
		}
		contents, err := os.ReadFile(filename)
		if err != nil {
			return nil, nil, fmt.Errorf("named graph fixture %s: %w", name, err)
		}
		fixtures["the "+name+" graph"] = string(contents)
		digests[name+".cypher"] = actual
	}
	return fixtures, digests, nil
}

func readFeature(file *zip.File) (document featureDocument, returnErr error) {
	reader, err := file.Open()
	if err != nil {
		return featureDocument{}, err
	}
	defer func() {
		if err := reader.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	relative := strings.TrimPrefix(file.Name, "tck/features/")
	return parseFeature(relative, reader)
}

func verifyManifest(manifestPath string, result *report) error {
	contents, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest capabilityManifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		return err
	}
	if manifest.Release != "M23" || manifest.ArchiveSHA256 != result.ArchiveSHA256 {
		return fmt.Errorf("capability manifest release/checksum does not match the report")
	}
	statuses := make(map[string]string, len(result.Scenarios))
	for _, scenario := range result.Scenarios {
		statuses[scenario.ID] = scenario.FrontendStatus
	}
	for _, capability := range manifest.Cases {
		actual, exists := statuses[capability.ID]
		if !exists {
			return fmt.Errorf("manifest scenario does not exist: %s", capability.ID)
		}
		if actual != capability.Expected {
			return fmt.Errorf("manifest scenario %s: expected frontend %s, got %s", capability.ID, capability.Expected, actual)
		}
		if capability.Test == "" {
			return fmt.Errorf("manifest scenario %s is not tied to a local test", capability.ID)
		}
	}
	return nil
}

func fileSHA256(name string) (digest string, returnErr error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := file.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

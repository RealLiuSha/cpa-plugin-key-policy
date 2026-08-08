package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	_ "time/tzdata"

	"cpa-key-policy/internal/policy"
	policyPersist "cpa-key-policy/internal/policy/persist"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, time.Now); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, now func() time.Time) error {
	flags := flag.NewFlagSet("migrate-usage", flag.ContinueOnError)
	flags.SetOutput(stderr)
	statePath := flags.String("state", "", "path to the v1 or v2 state file")
	outPath := flags.String("out", "", "output usage file (default: state directory/cpa-key-policy-usage.json)")
	dryRun := flags.Bool("dry-run", false, "print the migration report without writing")
	timezone := flags.String("timezone", "Asia/Shanghai", "IANA accounting timezone")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if strings.TrimSpace(*statePath) == "" {
		return fmt.Errorf("-state is required")
	}
	location, err := time.LoadLocation(strings.TrimSpace(*timezone))
	if err != nil {
		return fmt.Errorf("load timezone: %w", err)
	}
	resolved, err := policy.ResolveStatePath(*statePath)
	if err != nil {
		return fmt.Errorf("resolve state path: %w", err)
	}
	output := strings.TrimSpace(*outPath)
	if output == "" {
		output = policyPersist.UsagePath(resolved)
	} else {
		output, err = filepath.Abs(output)
		if err != nil {
			return fmt.Errorf("resolve output path: %w", err)
		}
	}
	if samePathOrFile(resolved, output) {
		return fmt.Errorf("output usage path must differ from state path")
	}
	at := now()
	state, err := policy.LoadStateAt(resolved, at, location)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	usage := state.Usage
	if state.Version >= 2 && len(usage) == 0 {
		existingPath := policyPersist.UsagePath(resolved)
		existing, loadErr := policy.LoadUsage(existingPath)
		if os.IsNotExist(loadErr) && filepath.Clean(existingPath) != filepath.Clean(output) {
			existing, loadErr = policy.LoadUsage(output)
		}
		if loadErr == nil {
			usage = existing
		} else if !os.IsNotExist(loadErr) {
			return fmt.Errorf("load existing usage file: %w", loadErr)
		}
	}
	after := policy.SummarizeUsageStates(usage, at, location, location.String())
	before := state.PreMigrationUsageTotals()
	for id, totals := range after {
		if _, exists := before[id]; !exists {
			before[id] = totals
		}
	}
	report := struct {
		StateVersion int                                    `json:"state_version"`
		Migrated     bool                                   `json:"migrated"`
		Timezone     string                                 `json:"timezone"`
		Output       string                                 `json:"output"`
		BeforeTotals map[string]policy.UsageMigrationTotals `json:"before_totals"`
		AfterTotals  map[string]policy.UsageMigrationTotals `json:"after_totals"`
	}{
		StateVersion: state.Version,
		Migrated:     state.UsageMigrated(),
		Timezone:     location.String(),
		Output:       output,
		BeforeTotals: before,
		AfterTotals:  after,
	}
	if !*dryRun {
		if err := policy.SaveUsage(output, usage); err != nil {
			return fmt.Errorf("write usage: %w", err)
		}
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	return nil
}

func samePathOrFile(left, right string) bool {
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

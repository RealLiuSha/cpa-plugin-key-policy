package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	migration "cpa-key-policy/internal/migration/v2"
)

type repeatedStrings []string

func (values *repeatedStrings) String() string { return fmt.Sprintf("%v", []string(*values)) }
func (values *repeatedStrings) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, time.Now); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, now func() time.Time) error {
	flags := flag.NewFlagSet("migrate-model-schema", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var freeModels repeatedStrings
	options := migration.Options{}
	flags.StringVar(&options.StatePath, "state", "", "path to the v2 state JSON")
	flags.StringVar(&options.UsagePath, "usage", "", "path to the v2 usage JSON")
	flags.StringVar(&options.AuditPath, "audit", "", "path to the active v2 audit JSONL (defaults beside state)")
	flags.StringVar(&options.BackupDir, "backup-dir", "", "new or empty directory for the rollback package")
	flags.StringVar(&options.Timezone, "timezone", "Asia/Shanghai", "IANA timezone used for accounting windows")
	flags.BoolVar(&options.DryRun, "dry-run", false, "validate and print a deterministic report without writing files")
	flags.Var(&freeModels, "free-model", "v2 zero-price model confirmed to be intentionally free (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	options.FreeModels = append([]string(nil), freeModels...)
	return migration.Run(options, stdout, now)
}

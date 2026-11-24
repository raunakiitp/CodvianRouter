package main

// codvian-bench compression — runnable P1.4 compression & fidelity
// benchmark. The library lives in internal/compress/evalharness; this
// subcommand makes it executable without `go test`, so a measured
// table can be regenerated on any machine and archived as JSON.
//
// Exit codes: 0 = every case met its fidelity thresholds; 1 = a case
// fell below a threshold (or measurement failed); 2 = bad usage.

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/raunakiitp/codvian-router/internal/compress/evalharness"
)

func runCompression(args []string) int {
	return runCompressionOut(args, os.Stdout, os.Stderr)
}

func runCompressionOut(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("compression", flag.ContinueOnError)
	fs.SetOutput(errOut)
	model := fs.String("model", evalharness.DefaultModel, "model whose BPE tables are used for token counts")
	corpus := fs.String("corpus", "standard", "corpus to run: standard | long | all")
	repeats := fs.Int("repeats", 3, "latency samples per case/engine (>=1)")
	jsonOnly := fs.Bool("json", false, "print only the machine-readable JSON report")
	stableJSON := fs.Bool("stable", false, "with -json: omit machine-dependent latency fields (byte-reproducible)")
	failOnViolation := fs.Bool("fail-on-violation", true, "exit 1 when any case falls below its fidelity thresholds")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(errOut, "codvian-bench compression: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	var cases []evalharness.Case
	switch *corpus {
	case "standard":
		cases = evalharness.Corpus()
	case "long":
		cases = evalharness.LongContextCorpus()
	case "all":
		cases = append(append([]evalharness.Case{}, evalharness.Corpus()...), evalharness.LongContextCorpus()...)
	default:
		fmt.Fprintf(errOut, "codvian-bench compression: unknown corpus %q (want standard|long|all)\n", *corpus)
		return 2
	}

	report, err := evalharness.RunBenchmark(evalharness.Options{
		Model:   *model,
		Cases:   cases,
		Repeats: *repeats,
	})
	if err != nil {
		fmt.Fprintf(errOut, "codvian-bench compression: measurement failed: %v\n", err)
		return 1
	}

	if *jsonOnly {
		var (
			b   []byte
			err error
		)
		if *stableJSON {
			b, err = report.JSONStable()
		} else {
			b, err = report.JSON()
		}
		if err != nil {
			fmt.Fprintf(errOut, "codvian-bench compression: JSON render failed: %v\n", err)
			return 1
		}
		out.Write(b)
		fmt.Fprintln(out)
	} else {
		fmt.Fprint(out, evalharness.RenderReportText(report))
	}

	failed := 0
	for _, t := range report.Totals {
		failed += t.Failed
	}
	if failed > 0 {
		fmt.Fprintf(errOut, "codvian-bench compression: %d case-engine row(s) below their fidelity thresholds\n", failed)
		if *failOnViolation {
			return 1
		}
	}
	return 0
}

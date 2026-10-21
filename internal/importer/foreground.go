package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/robertjndw/gotero/internal/adminapi"
	"github.com/robertjndw/gotero/internal/repo"
)

// RunForeground drives a queued job to its end in the calling process and
// prints a line whenever the progress changes. It is for the command line,
// where nobody polls the web UI. If a running server claims the job first,
// the function just follows it.
func (w *Worker) RunForeground(ctx context.Context, jobID int64, out io.Writer) error {
	imports := w.Store.Imports()
	runErr := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			j, err := imports.Get(ctx, jobID)
			if err != nil {
				runErr <- err
				return
			}
			if finished(j.Status) {
				runErr <- nil
				return
			}
			did, err := w.RunOnce(ctx)
			if err != nil {
				runErr <- err
				return
			}
			if !did {
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
		}
		runErr <- ctx.Err()
	}()

	var last string
	report := func() {
		j, err := imports.Get(context.WithoutCancel(ctx), jobID)
		if err != nil {
			return
		}
		if line := progressLine(j); line != last {
			last = line
			_, _ = fmt.Fprintln(out, line)
		}
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-runErr:
			report()
			if err != nil {
				if ctx.Err() != nil {
					_, _ = fmt.Fprintln(out, "Interrupted. The job stays queued and continues when it is started again.")
				}
				return err
			}
			j, gerr := imports.Get(context.WithoutCancel(ctx), jobID)
			if gerr != nil {
				return gerr
			}
			switch j.Status {
			case "failed":
				return fmt.Errorf("import failed: %s", j.Error)
			case "cancelled":
				return errors.New("import was cancelled")
			}
			if j.Error != "" {
				_, _ = fmt.Fprintln(out, "Warning:", j.Error)
			}
			return nil
		case <-tick.C:
			report()
		}
	}
}

func finished(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

func progressLine(j repo.ImportJob) string {
	var p adminapi.ImportProgress
	if len(j.Progress) > 0 {
		_ = json.Unmarshal(j.Progress, &p)
	}
	done := 0
	for _, l := range p.Libraries {
		if l.Done {
			done++
		}
	}
	return fmt.Sprintf("%s: objects %d/%d, files %d/%d, %s, libraries %d/%d",
		j.Status, p.Objects, p.Total, p.Files, p.FilesAll, humanBytes(p.Bytes), done, len(p.Libraries))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

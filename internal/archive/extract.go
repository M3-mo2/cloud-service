package archive

import (
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"

	"sandboxd/internal/model"
)

// Sink receives validated entries. The service provides a workspace-backed
// implementation (quota-accounted, symlink-safe); unit tests may provide a
// fake. The extraction engine never touches the filesystem directly.
type Sink interface {
	// Mkdir creates a directory at rel (slash path relative to the destination
	// root). Creating an existing directory is a no-op.
	Mkdir(rel string, mode fs.FileMode) error
	// Open opens rel for streaming. overwrite=false and an existing path must
	// return ErrEntryExists; a write past the workspace's disk allowance must
	// return a disk error (the service normalizes ENOSPC/quota to one).
	Open(rel string, mode fs.FileMode, overwrite bool) (io.WriteCloser, error)
}

// ErrBudgetTotal is returned when a write would exceed the total byte budget.
var ErrBudgetTotal = errors.New("archive exceeds total size budget")

// Result reports what an extraction (or a dry run) would do / did. A dry run
// returns the same shape with BytesWritten 0 and DryRun true.
type Result struct {
	Format           string  `json:"format"`
	Destination      string  `json:"destination"`
	EntriesTotal     int     `json:"entries_total"`
	EntriesExtracted int     `json:"entries_extracted"`
	EntriesSkipped   int     `json:"entries_skipped"`
	EntriesRejected  int     `json:"entries_rejected"`
	BytesWritten     int64   `json:"bytes_written"`
	Entries          []Entry `json:"entries"`
	DryRun           bool    `json:"dry_run"`
}

// ErrBudgetEntry is returned when a write would exceed a single entry's budget.
var ErrBudgetEntry = errors.New("archive entry exceeds size budget")

// Limits derived from Options, resolved once.
type limits struct {
	maxEntries int
	maxTotal   int64
	maxFile    int64
	maxRatio   int
	overwrite  bool
	skipJunk   bool
}

func limitsFrom(opts Options) limits {
	return limits{
		maxEntries: opts.MaxEntries,
		maxTotal:   opts.MaxTotalBytes,
		maxFile:    opts.MaxFileBytes,
		maxRatio:   opts.MaxRatio,
		overwrite:  opts.Overwrite,
		skipJunk:   opts.SkipJunk,
	}
}

// Extract validates and extracts an archive. destRel is the cleaned,
// slash-relative destination directory inside the workspace ("" or "." = root).
// clean is workspace.CleanRelative, applied to every entry name. dryRun lists
// the entries and writes nothing.
//
// The archive header list is validated in full BEFORE any byte is written, so a
// refused archive leaves no partial tree. Bytes actually written are bounded by
// a streaming budget, so a lying header cannot overshoot.
func Extract(data []byte, opts Options, destRel string, sink Sink, clean func(string) (string, error), dryRun bool) (Result, error) {
	lim := limitsFrom(opts)
	destRel = normalizeDest(destRel)

	res := Result{
		Destination: destRel,
		DryRun:      dryRun,
		Entries:     []Entry{},
	}

	// ---- Pass 1: read and validate every header, writing nothing ----------
	rdr, format, err := openArchive(data, opts)
	if err != nil {
		return res, err
	}
	defer rdr.close()
	res.Format = string(format)

	raws, totalDeclared, err := collectHeaders(rdr, lim)
	if err != nil {
		return res, err
	}
	// Encryption and multivolume are archive-level and are refused outright
	// (they are not "unsafe entries"; they cannot be extracted at all).
	for _, r := range raws {
		if r.Encrypted {
			return res, errEncrypted()
		}
	}
	if lim.maxTotal > 0 && totalDeclared > lim.maxTotal {
		return res, errTooLarge("max_total_mb")
	}

	plan := buildPlan(raws, opts, destRel, clean)
	res.EntriesTotal = len(plan)

	// Identify the first refusal, if any.
	var firstReject string
	for _, pe := range plan {
		res.Entries = append(res.Entries, pe.entry)
		if pe.reject != "" && firstReject == "" {
			firstReject = pe.reject
		}
	}

	if dryRun {
		// A dry run reports decided/rejected entries and writes nothing. It
		// does NOT fail on a rejected entry: previewing a hostile archive is
		// exactly its purpose.
		for _, pe := range plan {
			switch {
			case pe.reject != "":
				res.EntriesRejected++
			case pe.entry.Skipped:
				res.EntriesSkipped++
			default:
				res.EntriesExtracted++
			}
		}
		res.BytesWritten = 0
		return res, nil
	}

	// A real extraction refuses the WHOLE archive if any entry is rejected, so
	// nothing partial is left behind.
	if firstReject != "" {
		return res, refusalError(firstReject, plan)
	}

	// ---- Pass 2: stream the data under a budget ---------------------------
	// A fresh reader, so a header-only RAR solid pass does not interfere with
	// the data pass (solid files must be read in order; this pass does).
	rdr2, _, err := openArchive(data, opts)
	if err != nil {
		return res, err
	}
	defer rdr2.close()

	idx := 0
	var written int64
	for {
		_, nerr := rdr2.next()
		if nerr == io.EOF {
			break
		}
		if nerr != nil {
			return res, nerr
		}
		if idx >= len(plan) {
			return res, corrupt(errors.New("archive changed between passes"))
		}
		pe := plan[idx]
		idx++
		if pe.reject != "" || pe.entry.Skipped {
			continue
		}
		switch pe.raw.Kind {
		case KindDir:
			if err := sink.Mkdir(pe.rel, SanitizeMode(pe.raw.Mode)); err != nil {
				return res, err
			}
			res.EntriesExtracted++
		case KindFile:
			rc, oerr := rdr2.open()
			if oerr != nil {
				return res, oerr
			}
			wc, werr := sink.Open(pe.rel, SanitizeMode(pe.raw.Mode), lim.overwrite)
			if werr != nil {
				_ = rc.Close()
				if errors.Is(werr, ErrEntryExists) {
					return res, errEntryExists(pe.entry.Path)
				}
				return res, werr
			}
			entryLimit := lim.maxFile
			if entryLimit <= 0 {
				entryLimit = 1 << 62
			}
			n, cerr := copyBudgeted(wc, rc, &written, lim.maxTotal, entryLimit)
			closeErr := wc.Close()
			_ = rc.Close()
			if cerr != nil {
				return res, mapCopyErr(cerr, pe, lim, n)
			}
			if closeErr != nil {
				return res, closeErr
			}
			res.EntriesExtracted++
		}
	}
	res.EntriesExtracted = 0
	res.EntriesSkipped = 0
	res.EntriesRejected = 0
	for _, pe := range plan {
		switch {
		case pe.reject != "":
			res.EntriesRejected++
		case pe.entry.Skipped:
			res.EntriesSkipped++
		default:
			res.EntriesExtracted++
		}
	}
	res.BytesWritten = written
	return res, nil
}

// collectHeaders reads every header, enforcing the entry-count cap, and sums
// the declared uncompressed sizes. No data is consumed (header-only pass), so
// it is O(n) in the number of entries and cannot be made quadratic by a long
// extraction. A nil reader or an over-count is a stable refusal.
func collectHeaders(rdr reader, lim limits) ([]rawEntry, int64, error) {
	var raws []rawEntry
	var totalDeclared int64
	for {
		r, err := rdr.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		raws = append(raws, r)
		if lim.maxEntries > 0 && len(raws) > lim.maxEntries {
			return nil, 0, errTooManyEntries(lim.maxEntries)
		}
		if r.Kind == KindFile && r.SizeKnown && r.Uncompressed > 0 {
			totalDeclared += r.Uncompressed
		}
	}
	return raws, totalDeclared, nil
}

// copyBudgeted copies from r to w, never writing more than the total budget or
// the entry budget. It returns the number of bytes written for THIS entry and
// ErrBudgetTotal / ErrBudgetEntry when a limit is reached. It writes at most
// the allowed bytes, so the on-disk total can never exceed the budget.
func copyBudgeted(w io.Writer, r io.Reader, totalUsed *int64, totalLimit, entryLimit int64) (int64, error) {
	buf := make([]byte, 32*1024)
	var entryWritten int64
	for {
		nr, rerr := r.Read(buf)
		if nr > 0 {
			chunk := buf[:nr]
			// Total budget.
			if totalLimit > 0 {
				remaining := totalLimit - *totalUsed
				if remaining <= 0 {
					return entryWritten, ErrBudgetTotal
				}
				if int64(len(chunk)) > remaining {
					allowed := int(remaining)
					if allowed > 0 {
						nw, werr := w.Write(chunk[:allowed])
						*totalUsed += int64(nw)
						entryWritten += int64(nw)
						if werr != nil {
							return entryWritten, werr
						}
					}
					return entryWritten, ErrBudgetTotal
				}
			}
			// Entry budget.
			if entryWritten+int64(len(chunk)) > entryLimit {
				allowed := entryLimit - entryWritten
				if allowed > 0 {
					nw, werr := w.Write(chunk[:allowed])
					*totalUsed += int64(nw)
					entryWritten += int64(nw)
					if werr != nil {
						return entryWritten, werr
					}
				}
				return entryWritten, ErrBudgetEntry
			}
			nw, werr := w.Write(chunk)
			*totalUsed += int64(nw)
			entryWritten += int64(nw)
			if werr != nil {
				return entryWritten, werr
			}
			if nw < len(chunk) {
				return entryWritten, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return entryWritten, nil
			}
			return entryWritten, rerr
		}
	}
}

// mapCopyErr converts a copy failure into the right stable error. A disk error
// from the sink is passed through untouched (the service maps it to a disk
// error); the budget errors become archive_too_large.
func mapCopyErr(err error, pe planEntry, lim limits, written int64) error {
	switch {
	case errors.Is(err, ErrBudgetTotal), errors.Is(err, ErrBudgetEntry):
		return errTooLarge("max_total_mb or max_file_mb")
	case errors.Is(err, ErrEntryExists):
		return errEntryExists(pe.entry.Path)
	default:
		return err
	}
}

// refusalError builds the stable refusal for a rejected plan entry.
func refusalError(code string, plan []planEntry) error {
	name := ""
	for _, pe := range plan {
		if pe.reject == code {
			name = pe.entry.Path
			break
		}
	}
	switch code {
	case model.CodeUnsupportedArchive:
		return newError(model.CodeUnsupportedArchive, "unsupported archive format")
	case model.CodeArchiveTooLarge:
		return errTooLarge("max_file_mb")
	case model.CodeArchiveTooManyEntries:
		return errTooManyEntries(0)
	case model.CodeArchiveRatioExceeded:
		return errRatio(name)
	case model.CodeArchiveEntryExists:
		return errEntryExists(name)
	default:
		return errUnsafe(name)
	}
}

// normalizeDest cleans a destination into a slash-relative path (no leading
// slash, no trailing slash). "" and "." both mean the workspace root.
func normalizeDest(destRel string) string {
	destRel = strings.ReplaceAll(destRel, "\\", "/")
	destRel = strings.Trim(destRel, "/")
	if destRel == "." {
		return ""
	}
	return path.Clean(destRel)
}

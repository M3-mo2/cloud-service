package archive

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"io/fs"

	"github.com/nwaples/rardecode/v2"

	"sandboxd/internal/model"
)

// reader abstracts a ZIP or RAR reader for the header pass and the data pass.
type reader interface {
	// next returns the next header, or io.EOF at the end. Implementations map
	// their own encryption / multivolume errors to the stable archive codes.
	next() (rawEntry, error)
	// open returns a reader for the current entry's data.
	open() (io.ReadCloser, error)
	close() error
}

// openArchive detects the format from content and returns a reader plus the
// detected format. Refusals use the stable codes.
func openArchive(data []byte, opts Options) (reader, Format, error) {
	format, sig := Detect(data)
	if format == "" {
		return nil, "", errUnsupported(sig)
	}
	if !opts.FormatAllowed(format) {
		return nil, "", newErrorDetails(model.CodeUnsupportedArchive,
			"archive format disabled by configuration", map[string]string{"format": string(format)})
	}
	switch format {
	case FormatZip:
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, "", corrupt(err)
		}
		return &zipReader{zr: zr}, format, nil
	case FormatRAR:
		// rardecode.NewReader reports an encrypted archive (header encryption)
		// at construction; multi-volume and entry encryption surface on Next.
		rr, err := rardecode.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, "", mapRarErr(err)
		}
		return &rarReader{r: rr}, format, nil
	}
	return nil, "", errUnsupported(sig)
}

// corrupt maps a low-level parse failure to a clear, non-fatal error. The
// archive content was recognised by magic bytes but could not be parsed, so it
// is reported as unprocessable rather than unsupported. The daemon is never
// affected: this is an ordinary returned error.
func corrupt(err error) *Error {
	return newErrorDetails(model.CodeUnprocessable, "corrupt or truncated archive",
		map[string]string{"error": err.Error()})
}

// mapRarErr maps a rardecode sentinel/error to the stable code or a corrupt
// error. It is used both at construction and during Next.
func mapRarErr(err error) *Error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, rardecode.ErrArchiveEncrypted),
		errors.Is(err, rardecode.ErrArchivedFileEncrypted):
		return errEncrypted()
	case errors.Is(err, rardecode.ErrMultiVolume):
		return errMultivolume()
	default:
		return corrupt(err)
	}
}

// zipReader adapts archive/zip to the reader interface.
type zipReader struct {
	zr     *zip.Reader
	cur    *zip.File
	opened io.ReadCloser
}

func (z *zipReader) next() (rawEntry, error) {
	if z.opened != nil {
		_ = z.opened.Close()
		z.opened = nil
	}
	if len(z.zr.File) == 0 {
		return rawEntry{}, io.EOF
	}
	f := z.zr.File[0]
	z.zr.File = z.zr.File[1:]
	z.cur = f
	fmode := f.Mode()
	kind := KindFile
	switch {
	case f.FileInfo().IsDir():
		kind = KindDir
	case fmode&fs.ModeSymlink != 0:
		kind = KindSymlink
	case fmode&fs.ModeDevice != 0, fmode&fs.ModeNamedPipe != 0:
		// Devices, FIFOs and sockets are refused like links; classify as
		// hardlink is not accurate, so reuse symlink semantics via KindSymlink
		// is not right either -- fall through to file and let the sink write a
		// plain file is also wrong. Mark as a symlink-shaped refusal.
		kind = KindSymlink
	}
	// ZIP encrypts with general-purpose bit 0 (and strong encryption in bit 6).
	encrypted := f.Flags&0x1 != 0 || f.Flags&0x40 != 0
	return rawEntry{
		Name:         f.Name,
		Kind:         kind,
		Mode:         f.Mode(),
		Uncompressed: int64(f.UncompressedSize64),
		Compressed:   int64(f.CompressedSize64),
		SizeKnown:    true,
		Encrypted:    encrypted,
	}, nil
}

func (z *zipReader) open() (io.ReadCloser, error) {
	if z.cur == nil {
		return nil, io.EOF
	}
	rc, err := z.cur.Open()
	if err != nil {
		return nil, err
	}
	z.opened = rc
	return rc, nil
}

func (z *zipReader) close() error {
	if z.opened != nil {
		_ = z.opened.Close()
		z.opened = nil
	}
	return nil
}

// rarReader adapts rardecode to the reader interface.
type rarReader struct {
	r   *rardecode.Reader
	cur *rardecode.FileHeader
}

func (rr *rarReader) next() (rawEntry, error) {
	h, err := rr.r.Next()
	if err == io.EOF {
		return rawEntry{}, io.EOF
	}
	if err != nil {
		return rawEntry{}, mapRarErr(err)
	}
	rr.cur = h
	kind := KindFile
	switch {
	case h.IsDir:
		kind = KindDir
	case h.LinkType == rardecode.LinkTypeUnixSymlink ||
		h.LinkType == rardecode.LinkTypeWindowsSymlink ||
		h.LinkType == rardecode.LinkTypeWindowsJunction:
		kind = KindSymlink
	case h.LinkType == rardecode.LinkTypeHardLink || h.LinkType == rardecode.LinkTypeFileCopy:
		kind = KindHardlink
	}
	// A link from an archive is NEVER materialized; the kind is only used to
	// refuse it clearly (see plan.go / ADR 92).
	return rawEntry{
		Name:         h.Name,
		Kind:         kind,
		LinkTarget:   h.LinkTarget,
		Mode:         h.Mode(),
		Uncompressed: h.UnPackedSize,
		Compressed:   h.PackedSize,
		SizeKnown:    !h.UnKnownSize,
		Encrypted:    h.Encrypted || h.HeaderEncrypted,
	}, nil
}

func (rr *rarReader) open() (io.ReadCloser, error) {
	return io.NopCloser(rr.r), nil
}

func (rr *rarReader) close() error { return nil }

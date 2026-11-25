package build

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"

	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/internal/operation"
	"github.com/28Pollux28/galvanize/internal/pathutil"
)

// writeArchive packs the challenge folder (without .git, and without
// symlinks leading outside it) into a temporary tar.gz, plus the build
// compose file. Returns the archive path and the folder to build in,
// relative to the archive root.
func writeArchive(c *challenge.Challenge, plan Plan, log operation.Logger) (string, string, error) {
	f, err := os.CreateTemp("", "galvanize-build-*.tar.gz")
	if err != nil {
		return "", "", err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	// On failure the archive is incomplete anyway: errors closing or
	// removing it change nothing
	fail := func(err error) (string, string, error) {
		_ = tw.Close()
		_ = gz.Close()
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", "", err
	}

	walkErr := filepath.Walk(c.Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(c.Dir, path)
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}
		name := filepath.ToSlash(filepath.Join("ctx", rel))
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if _, err := pathutil.Resolve(c.Dir, path); err != nil {
				log.Warnf("[%s] Skipped symlink leading outside the challenge folder: %s", Label(c), rel)
				return nil
			}
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()
		_, err = io.Copy(tw, src)
		return err
	})
	if walkErr != nil {
		return fail(walkErr)
	}

	workdir := "ctx"
	if plan.Kind == KindCompose {
		rel, err := filepath.Rel(c.Dir, plan.composeDir)
		if err != nil {
			return fail(err)
		}
		workdir = filepath.ToSlash(filepath.Join("ctx", rel))
		hdr := &tar.Header{Name: workdir + "/" + buildComposeName, Mode: 0o644, Size: int64(len(plan.composeDoc)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return fail(err)
		}
		if _, err := tw.Write(plan.composeDoc); err != nil {
			return fail(err)
		}
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", "", err
	}
	return f.Name(), workdir, nil
}

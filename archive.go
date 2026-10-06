package mockgithub

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"maps"
	"net/http"
)

func archiveResponse(repo *repo, commit Commit, format string) apiResponse {
	name := text(repo.data["name"])
	sha := commit.SHA
	files := maps.Clone(commit.Files)
	return func(w http.ResponseWriter) int { return serveArchive(w, name, sha, files, format) }
}

func serveArchive(w http.ResponseWriter, name, sha string, files map[string]string, format string) int {
	var buffer bytes.Buffer
	root := name + "-" + sha[:7] + "/"
	var err error
	if format == "zipball" {
		archive := zip.NewWriter(&buffer)
		_, err = archive.Create(root)
		for _, path := range sortedKeys(files) {
			file, createErr := archive.Create(root + path)
			if createErr != nil {
				err = createErr
				break
			}
			if _, err = file.Write([]byte(files[path])); err != nil {
				break
			}
		}
		if closeErr := archive.Close(); err == nil {
			err = closeErr
		}
		w.Header().Set("Content-Type", "application/zip")
	} else {
		compressed := gzip.NewWriter(&buffer)
		archive := tar.NewWriter(compressed)
		err = archive.WriteHeader(&tar.Header{Name: root, Mode: 0755, Typeflag: tar.TypeDir})
		for _, path := range sortedKeys(files) {
			content := files[path]
			if err = archive.WriteHeader(&tar.Header{Name: root + path, Mode: 0644, Size: int64(len(content))}); err != nil {
				break
			}
			if _, err = archive.Write([]byte(content)); err != nil {
				break
			}
		}
		if closeErr := archive.Close(); err == nil {
			err = closeErr
		}
		if closeErr := compressed.Close(); err == nil {
			err = closeErr
		}
		w.Header().Set("Content-Type", "application/gzip")
	}
	if err != nil {
		return writeError(w, 500, "Cannot generate source archive")
	}
	extension := ".zip"
	if format == "tarball" {
		extension = ".tar.gz"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+extension))
	w.Header().Set("Content-Length", fmt.Sprint(buffer.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buffer.Bytes())
	return http.StatusOK
}

package bundle

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/suborbital/appspec/tenant"
)

// Bundle represents a Runnable bundle.
type Bundle struct {
	filepath     string
	TenantConfig *tenant.Config
	staticFiles  map[string]bool
}

// StaticFile returns a static file from the bundle, if it exists.
func (b *Bundle) StaticFile(filePath string) ([]byte, error) {
	// normalize in case the caller added `/` or `./` to the filename.
	filePath = NormalizeStaticFilename(filePath)

	if _, exists := b.staticFiles[filePath]; !exists {
		return nil, os.ErrNotExist
	}

	r, err := zip.OpenReader(b.filepath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open bundle")
	}

	defer r.Close()

	// re-add the static/ prefix to ensure sandboxing to the static directory.
	staticFilePath := ensurePrefix(filePath, "static/")

	var contents []byte

	for _, f := range r.File {
		if f.Name == staticFilePath {
			file, err := f.Open()
			if err != nil {
				return nil, errors.Wrap(err, "failed to Open static file")
			}

			defer file.Close()

			contents, err = ioutil.ReadAll(file)
			if err != nil {
				return nil, errors.Wrap(err, "failed to ReadAll static file")
			}

			break
		}
	}

	return contents, nil
}

// Write writes a runnable bundle
// based loosely on https://golang.org/src/archive/zip/example_test.go
// staticFiles should be a map of *relative* filepaths to their associated files, with or without the `static/` prefix.
func Write(tenantConfigBytes []byte, modules []os.File, staticFiles map[string]os.File, targetPath string) error {
	if tenantConfigBytes == nil || len(tenantConfigBytes) == 0 {
		return errors.New("tenant config must be provided")
	}

	// Create a buffer to write our archive to.
	buf := new(bytes.Buffer)

	// Create a new zip archive.
	w := zip.NewWriter(buf)

	// Add tenant config to archive.
	if err := writeTenantConfig(w, tenantConfigBytes); err != nil {
		return errors.Wrap(err, "failed to writeTenantConfig")
	}

	// Add the Wasm modules to the archive.
	for _, file := range modules {
		if file.Name() == "tenant.yaml" || file.Name() == "tenant.yml" {
			// only allow the canonical tenant config that's passed in.
			continue
		}

		contents, err := ioutil.ReadAll(&file)
		if err != nil {
			return errors.Wrapf(err, "failed to read file %s", file.Name())
		}

		if err := writeFile(w, filepath.Base(file.Name()), contents); err != nil {
			return errors.Wrap(err, "failed to writeFile into bundle")
		}
	}

	// Add static files to the archive.
	for path, file := range staticFiles {
		contents, err := ioutil.ReadAll(&file)
		if err != nil {
			return errors.Wrapf(err, "failed to read file %s", file.Name())
		}

		fileName := ensurePrefix(path, "static/")
		if err := writeFile(w, fileName, contents); err != nil {
			return errors.Wrap(err, "failed to writeFile into bundle")
		}
	}

	if err := w.Close(); err != nil {
		return errors.Wrap(err, "failed to close bundle writer")
	}

	if err := ioutil.WriteFile(targetPath, buf.Bytes(), 0777); err != nil {
		return errors.Wrap(err, "failed to write bundle to disk")
	}

	return nil
}

func writeTenantConfig(w *zip.Writer, tenantConfigBytes []byte) error {
	if err := writeFile(w, "tenant.yaml", tenantConfigBytes); err != nil {
		return errors.Wrap(err, "failed to writeFile for tenant.yaml")
	}

	return nil
}

func writeFile(w *zip.Writer, name string, contents []byte) error {
	f, err := w.Create(name)
	if err != nil {
		return errors.Wrap(err, "failed to add file to bundle")
	}

	_, err = f.Write(contents)
	if err != nil {
		return errors.Wrap(err, "failed to write file into bundle")
	}

	return nil
}

// Read reads a .wasm.zip file and returns the bundle of wasm modules
// (suitable to be loaded into a wasmer instance).
func Read(path string) (*Bundle, error) {
	// Open a zip archive for reading.
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open bundle")
	}

	defer r.Close()

	bundle := &Bundle{
		filepath:    path,
		staticFiles: map[string]bool{},
	}

	// first, find the tenant config.
	for _, f := range r.File {
		if f.Name == "tenant.yaml" {
			tenantConfig, err := readTenantConfig(f)
			if err != nil {
				return nil, errors.Wrap(err, "failed to readTenantConfig from bundle")
			}

			bundle.TenantConfig = tenantConfig
			continue
		}
	}

	if bundle.TenantConfig == nil {
		return nil, errors.New("bundle is missing tenant.yaml")
	}

	// Iterate through the files in the archive.
	for _, f := range r.File {
		if f.Name == "tenant.yaml" {
			// we already have a tenant config by now.
			continue
		} else if strings.HasPrefix(f.Name, "static/") {
			// build up the list of available static files in the bundle for quick reference later.
			filePath := strings.TrimPrefix(f.Name, "static/")
			bundle.staticFiles[filePath] = true
			continue
		} else if !strings.HasSuffix(f.Name, ".wasm") {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return nil, errors.Wrapf(err, "failed to open %s from bundle", f.Name)
		}

		defer rc.Close()

		wasmBytes, err := ioutil.ReadAll(rc)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to read %s from bundle", f.Name)
		}

		runnable := bundle.TenantConfig.FindModule(strings.TrimSuffix(f.Name, ".wasm"))
		if runnable == nil {
			return nil, fmt.Errorf("unable to find Runnable for module %s", f.Name)
		}

		runnable.WasmRef = tenant.NewWasmModuleRef(f.Name, runnable.FQMN, wasmBytes)
	}

	if bundle.TenantConfig == nil {
		return nil, errors.New("bundle did not contain tenantConfig")
	}

	return bundle, nil
}

func readTenantConfig(f *zip.File) (*tenant.Config, error) {
	file, err := f.Open()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to open %s from bundle", f.Name)
	}

	tenantConfigBytes, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read %s from bundle", f.Name)
	}

	d := &tenant.Config{}
	if err := d.Unmarshal(tenantConfigBytes); err != nil {
		return nil, errors.Wrap(err, "failed to Unmarshal tenant config")
	}

	return d, nil
}

func ensurePrefix(val, prefix string) string {
	if strings.HasPrefix(val, prefix) {
		return val
	}

	return fmt.Sprintf("%s%s", prefix, val)
}

// NormalizeStaticFilename will take various variations of a filename and
// normalize it to what is listed in the staticFile name cache on the Bundle struct.
func NormalizeStaticFilename(fileName string) string {
	withoutStatic := strings.TrimPrefix(fileName, "static/")
	withoutLeadingSlash := strings.TrimPrefix(withoutStatic, "/")
	withoutDotSlash := strings.TrimPrefix(withoutLeadingSlash, "./")

	return withoutDotSlash
}

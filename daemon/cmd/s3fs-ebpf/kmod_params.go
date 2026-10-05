package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// kmodParamDir is where the s3fs_kfunc module exposes its parameters.
// A var so tests can point it at a temp dir.
var kmodParamDir = "/sys/module/s3fs_kfunc/parameters"

// setKmodParam writes one writable (0644) module parameter of the
// loaded kmod, e.g. softcap / capacity_bytes (S-02) or nocache.
func setKmodParam(name, value string) error {
	p := filepath.Join(kmodParamDir, name)
	// No O_CREATE: the parameter must already exist (module loaded).
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err == nil {
		_, err = f.WriteString(value)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		return fmt.Errorf("set kmod param %s=%s (is s3fs_kfunc loaded?): %w", name, value, err)
	}
	fmt.Printf("[kmod] %s=%s\n", name, value)
	return nil
}

package assetmatrix

import (
	"bytes"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type JSXAsset struct {
	input    Asset
	r        *AssetRoot
	p        string
	indexKey string
}

func (a *JSXAsset) OutputExt() string {
	return ".js"
}

func (a *JSXAsset) OutputPath() string {
	p, err := a.RelPath()
	if err != nil {
		log.Fatal(err)
	}
	if filepath.Ext(p) == ".jsx" {
		p = strings.TrimSuffix(p, ".jsx")
	}
	if filepath.Ext(p) != ".js" {
		return p + ".js"
	}
	return p
}

func (a *JSXAsset) Open() (*os.File, error) {
	return a.input.Open()
}

func (a *JSXAsset) Initialize() error {
	return nil
}

func (a *JSXAsset) Checksum() string {
	return a.input.Checksum()
}

func (a *JSXAsset) Path() string {
	return a.p
}

func (a *JSXAsset) RelPath() (string, error) {
	return filepath.Rel(a.r.Path, a.p)
}

func (a *JSXAsset) SetIndexKey(key string) {
	a.indexKey = key
}

func (a *JSXAsset) IndexKey() string {
	return a.indexKey
}

func (a *JSXAsset) ImportPaths() []string {
	return []string{}
}

func (a *JSXAsset) Compile() (io.Reader, error) {
	data, err := a.input.Compile()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	cmd := exec.Command("node_modules/react-tools/bin/jsx", "--es6module")
	cmd.Stdin = data
	cmd.Stdout = &buf
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return &buf, nil
}

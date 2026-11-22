package github

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// App is one GitHub App installed on one organization of one host.
type App struct {
	// Host is the GitHub host: github.com, or a GitHub Enterprise Server host.
	Host string
	// Org is the organization (or user) that owns the repositories.
	Org string
	// AppID is the GitHub App's id.
	AppID string
	// InstallationID is the installation's id; when zero it is looked up with GET /orgs/{org}/installation.
	InstallationID int64
	// Key signs the App JWT.
	Key *rsa.PrivateKey
	// APIURL overrides the API base (https://api.github.com for github.com, https://<host>/api/v3 otherwise).
	APIURL string
}

// fileApp is one entry of the apps file (AFS_GITHUB_APPS_FILE).
type fileApp struct {
	Host           string `json:"host"`
	Org            string `json:"org"`
	AppID          string `json:"appId"`
	InstallationID int64  `json:"installationId"`
	// PrivateKeyFile is the PEM file of the app's private key, relative to the apps file's directory or absolute.
	PrivateKeyFile string `json:"privateKeyFile"`
	APIURL         string `json:"apiUrl"`
}

// LoadApps reads the apps file:
//
//	{"apps": [{"host": "github.com", "org": "acme", "appId": "123456", "installationId": 7890123,
//	           "privateKeyFile": "acme.pem"}]}
//
// installationId and apiUrl are optional. Every key file is read and parsed now, so a bad deployment fails at
// start, not at the first poll. Two entries for the same host and organization are refused.
func LoadApps(path string) ([]App, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the path is the operator's setting
	if err != nil {
		return nil, fmt.Errorf("github: read apps file: %w", err)
	}
	var f struct {
		Apps []fileApp `json:"apps"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("github: apps file %s: %w", filepath.Base(path), err)
	}
	seen := map[string]bool{}
	out := make([]App, 0, len(f.Apps))
	for i, a := range f.Apps {
		switch {
		case a.Host == "" || a.Org == "" || a.AppID == "" || a.PrivateKeyFile == "":
			return nil, fmt.Errorf("github: apps[%d] needs host, org, appId and privateKeyFile", i)
		case a.InstallationID < 0:
			return nil, fmt.Errorf("github: apps[%d].installationId is negative", i)
		}
		key := scopeKey(a.Host, a.Org)
		if seen[key] {
			return nil, fmt.Errorf("github: apps[%d] repeats %s", i, key)
		}
		seen[key] = true
		kp := a.PrivateKeyFile
		if !filepath.IsAbs(kp) {
			kp = filepath.Join(filepath.Dir(path), kp)
		}
		pemBytes, err := os.ReadFile(kp) //nolint:gosec // named by the operator's apps file
		if err != nil {
			return nil, fmt.Errorf("github: apps[%d] private key: %w", i, err)
		}
		k, err := ParseKey(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("github: apps[%d] private key: %w", i, err)
		}
		out = append(out, App{Host: strings.ToLower(a.Host), Org: a.Org, AppID: a.AppID, InstallationID: a.InstallationID, Key: k, APIURL: strings.TrimRight(a.APIURL, "/")})
	}
	return out, nil
}

// ParseKey parses an RSA private key in PEM (PKCS#1 or PKCS#8), as GitHub issues it.
func ParseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, errors.New("not a PEM file")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, errors.New("not an RSA private key")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA private key")
	}
	return rk, nil
}

func scopeKey(host, org string) string { return strings.ToLower(host + "/" + org) }

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// LookupSum fetches the go.sum lines for module@version from the checksum database
func LookupSum(gosumdb, module, version string) (zipHash string, err error) {
	if gosumdb == "" || strings.ToLower(gosumdb) == "off" {
		return "", nil
	}

	base := strings.TrimSuffix(gosumdb, "/")
	if base != "" && !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	lookupPath := module + "@" + version
	lookupURL := base + "/lookup/" + url.PathEscape(lookupPath)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(lookupURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sum db returned %d for %s", resp.StatusCode, lookupURL)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// Parse go.sum format. Response: record ID, then lines like:
	//   module version h1:zip_hash
	//   module version/go.mod h1:go.mod_hash
	// We want the line for module version (not /go.mod) - the h1:... is the zip hash.
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	prefix := module + " " + version + " "
	for _, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if strings.Contains(line, "/go.mod ") {
			continue
		}
		parts := strings.Fields(line)
		for i := len(parts) - 1; i >= 0; i-- {
			if strings.HasPrefix(parts[i], "h1:") {
				return parts[i], nil
			}
		}
	}
	return "", fmt.Errorf("zip hash not found for %s@%s", module, version)
}

// VerifyZip verifies zip data against the checksum database
func VerifyZip(gosumdb, module, version string, zipData []byte, gonosumdb []string) error {
	if gosumdb == "" || strings.ToLower(gosumdb) == "off" {
		return nil
	}

	for _, pattern := range gonosumdb {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		matched, err := path.Match(pattern, module)
		if err != nil {
			continue
		}
		if matched {
			return nil
		}
	}

	expectedHash, err := LookupSum(gosumdb, module, version)
	if err != nil {
		return fmt.Errorf("sum db lookup: %w", err)
	}
	if expectedHash == "" {
		return nil
	}

	sum := sha256.Sum256(zipData)
	actualHash := "h1:" + hex.EncodeToString(sum[:])

	if actualHash != expectedHash {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}
	return nil
}

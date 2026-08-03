package modeldiscovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const cacheDirectory = "provider-models"

func CachePath(configDir, provider string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, provider)
	name = strings.Trim(name, "._-")
	if name == "" {
		name = "provider"
	}
	digest := sha256.Sum256([]byte(provider))
	return filepath.Join(configDir, cacheDirectory, name+"-"+hex.EncodeToString(digest[:4])+".json")
}

func readCache(configDir string, provider llmProviderIdentity) (Snapshot, error) {
	path := CachePath(configDir, provider.Name)
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Snapshot{}, err
	}
	if snapshot.Version != 1 || !snapshot.Complete || len(snapshot.Models) == 0 || snapshot.Provider != provider.Name || snapshot.BaseURL != provider.BaseURL {
		return Snapshot{}, fmt.Errorf("provider model cache %s does not match provider configuration", path)
	}
	return snapshot, nil
}

// llmProviderIdentity keeps cache validation independent from credential fields.
type llmProviderIdentity struct {
	Name    string
	BaseURL string
}

func ReadProviderCache(configDir, name, baseURL string) (Snapshot, error) {
	return readCache(configDir, llmProviderIdentity{Name: name, BaseURL: baseURL})
}

func WriteCache(configDir string, snapshot Snapshot) error {
	if !snapshot.Complete || len(snapshot.Models) == 0 || snapshot.Provider == "" {
		return errors.New("cannot cache incomplete provider model snapshot")
	}
	dir := filepath.Join(configDir, cacheDirectory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := CachePath(configDir, snapshot.Provider)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func StateFromCache(snapshot Snapshot, now time.Time, ttl time.Duration) State {
	fresh := ttl > 0 && !snapshot.FetchedAt.IsZero() && now.Sub(snapshot.FetchedAt) <= ttl
	return State{Snapshot: snapshot, Authoritative: fresh}
}

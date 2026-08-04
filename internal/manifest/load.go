package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigFileNames are the repo-root files consulted for a Config document.
var ConfigFileNames = []string{"systemcd.yaml", "systemcd.yml", ".systemcd.yaml"}

// Load reads a repository rooted at dir. It reads repo settings from
// systemcd.yaml when present, then loads every manifest under the configured
// paths (or the whole tree when none are configured).
func Load(dir string) (*Repository, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("manifest root %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("manifest root %s is not a directory", dir)
	}

	repo := &Repository{Root: root, Config: DefaultConfig()}

	configPath, err := findConfig(root)
	if err != nil {
		return nil, err
	}
	if configPath != "" {
		docs, err := parseFile(configPath, root)
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			if d.Kind != ConfigKind {
				// A repo may keep ordinary resources in systemcd.yaml too.
				repo.Documents = append(repo.Documents, d)
				continue
			}
			cfg := DefaultConfig()
			if err := d.DecodeSpec(&cfg); err != nil {
				return nil, err
			}
			repo.Config = cfg
		}
	}

	searchPaths := repo.Config.Paths
	if len(searchPaths) == 0 {
		searchPaths = []string{"."}
	}

	loaded := map[string]bool{}
	if configPath != "" {
		loaded[configPath] = true
	}

	for _, p := range searchPaths {
		abs := filepath.Join(root, p)
		if err := walkPath(abs, root, loaded, repo); err != nil {
			return nil, err
		}
	}

	sort.SliceStable(repo.Documents, func(i, j int) bool {
		if repo.Documents[i].Source != repo.Documents[j].Source {
			return repo.Documents[i].Source < repo.Documents[j].Source
		}
		return repo.Documents[i].Index < repo.Documents[j].Index
	})
	return repo, nil
}

func findConfig(root string) (string, error) {
	for _, name := range ConfigFileNames {
		p := filepath.Join(root, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

func walkPath(abs, root string, loaded map[string]bool, repo *Repository) error {
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("path %s: %w", abs, err)
	}
	if !info.IsDir() {
		return loadInto(abs, root, loaded, repo)
	}
	return filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip VCS and asset directories; `files/` holds payloads
			// referenced by File.source, not manifests.
			switch d.Name() {
			case ".git", ".github", "files", "templates", "node_modules":
				if p != abs {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !isManifest(p) {
			return nil
		}
		return loadInto(p, root, loaded, repo)
	})
}

func isManifest(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	return ext == ".yaml" || ext == ".yml"
}

func loadInto(p, root string, loaded map[string]bool, repo *Repository) error {
	if loaded[p] {
		return nil
	}
	loaded[p] = true
	docs, err := parseFile(p, root)
	if err != nil {
		return err
	}
	for _, d := range docs {
		if d.Kind == ConfigKind {
			// Config outside the repo root is ambiguous; reject it loudly.
			return fmt.Errorf("%s: Config documents are only allowed in %s", d.Location(), strings.Join(ConfigFileNames, " or "))
		}
		repo.Documents = append(repo.Documents, d)
	}
	return nil
}

func parseFile(path, root string) ([]*Document, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rel, relErr := filepath.Rel(root, path)
	if relErr != nil {
		rel = path
	}
	return Parse(raw, rel)
}

// Parse decodes a possibly multi-document YAML stream. Empty documents (blank
// or comment-only, common around `---` separators) are skipped.
func Parse(data []byte, source string) ([]*Document, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []*Document
	for i := 0; ; i++ {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", source, i, err)
		}
		if node.Kind == 0 || (node.Kind == yaml.DocumentNode && len(node.Content) == 0) {
			continue
		}
		doc := &Document{Source: source, Index: i}
		if err := node.Decode(doc); err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", source, i, err)
		}
		if doc.Kind == "" && doc.APIVersion == "" && doc.Metadata.Name == "" {
			continue
		}
		out = append(out, doc)
	}
	return out, nil
}

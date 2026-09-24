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
	// Work from the real location, so a checkout reached through a symlink
	// and the paths walked beneath it agree on what "inside" means.
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
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
		var configDoc *Document
		for _, d := range docs {
			if d.Kind != ConfigKind {
				// A repo may keep ordinary resources in systemcd.yaml too.
				repo.Documents = append(repo.Documents, d)
				continue
			}
			if configDoc != nil {
				return nil, fmt.Errorf("%s: a second Config document; settings are already declared at %s", d.Location(), configDoc.Location())
			}
			configDoc = d
			if d.APIVersion != APIVersion {
				return nil, fmt.Errorf("%s: apiVersion %q is not supported (want %q)", d.Location(), d.APIVersion, APIVersion)
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
		if !within(root, abs) {
			return nil, fmt.Errorf("paths: %q is outside the repository", p)
		}
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
	found := ""
	for _, name := range ConfigFileNames {
		p := filepath.Join(root, name)
		if _, err := os.Stat(p); err == nil {
			// With two of them, which one holds the settings would depend
			// on the order of this list; say so instead of guessing.
			if found != "" {
				return "", fmt.Errorf("both %s and %s exist; keep one", filepath.Base(found), name)
			}
			found = p
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return found, nil
}

// within reports whether p is the (already resolved) root or lies beneath it,
// after resolving any symlinks in p. Anything else would load manifests the
// repository does not hold.
func within(root, p string) bool {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func walkPath(abs, root string, loaded map[string]bool, repo *Repository) error {
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("path %s: %w", abs, err)
	}
	// WalkDir does not descend into a root that is itself a symlink.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	if !info.IsDir() {
		return loadInto(abs, root, loaded, repo)
	}
	return filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Hidden entries are tooling (.git, .github, .circleci, editor lock
		// files), not manifests. A path listed explicitly is still loaded.
		if p != abs && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// Skip asset directories; `files/` holds payloads referenced by
			// File.source, not manifests.
			switch d.Name() {
			case "files", "templates", "node_modules":
				if p != abs {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !isManifest(p) {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 && !within(root, p) {
			return fmt.Errorf("%s: symlink resolves outside the repository", p)
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
		if doc.APIVersion == APIVersion {
			if err := checkKnownFields(&node); err != nil {
				return nil, fmt.Errorf("%s[%d]: %w", source, i, err)
			}
		}
		out = append(out, doc)
	}
	return out, nil
}

// knownFields lists the keys a document may use outside its spec. Decoding a
// yaml.Node has no strict mode, and a misspelling here is not harmless: a
// `target:` under metadata would otherwise be dropped and the document
// applied to every host in the fleet.
var knownFields = map[string][]string{
	"":                 {"apiVersion", "kind", "metadata", "spec", "dependsOn", "notify"},
	"metadata":         {"name", "labels", "targets"},
	"metadata.targets": {"hosts", "labels"},
}

func checkKnownFields(node *yaml.Node) error {
	return checkMapping(unwrapDocument(node), "")
}

func checkMapping(n *yaml.Node, path string) error {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i]
		if key.Value == "<<" {
			continue
		}
		known := false
		for _, name := range knownFields[path] {
			if key.Value == name {
				known = true
				break
			}
		}
		if !known {
			where := "the document"
			if path != "" {
				where = path
			}
			return fmt.Errorf("line %d: unknown field %q in %s", key.Line, key.Value, where)
		}
		child := key.Value
		if path != "" {
			child = path + "." + key.Value
		}
		if _, nested := knownFields[child]; nested {
			if err := checkMapping(n.Content[i+1], child); err != nil {
				return err
			}
		}
	}
	return nil
}

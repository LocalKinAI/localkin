package skill

import (
	"os"
	"path/filepath"
)

// WorkspaceAware skills resolve relative paths against — and run
// commands inside — the session's workspace directory. The kernel sets
// it at boot (cwd) and whenever the user picks a folder in the UI.
type WorkspaceAware interface {
	SetWorkspace(dir string)
}

// SetWorkspace propagates the workspace to every skill that cares.
func (r *Registry) SetWorkspace(dir string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.skills {
		if w, ok := s.(WorkspaceAware); ok {
			w.SetWorkspace(dir)
		}
	}
}

// resolvePath expands ~ and makes a relative path absolute against the
// workspace (or the process cwd when there is none).
func resolvePath(p, workspace string) (string, error) {
	p = expandTilde(p)
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	if workspace != "" {
		return filepath.Clean(filepath.Join(workspace, p)), nil
	}
	return filepath.Abs(p)
}

// ValidWorkspace reports whether dir exists and is a directory, returning
// its cleaned absolute form.
func ValidWorkspace(dir string) (string, error) {
	abs, err := filepath.Abs(expandTilde(dir))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", os.ErrInvalid
	}
	return abs, nil
}

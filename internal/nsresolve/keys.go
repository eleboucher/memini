package nsresolve

// Pin-key prefixes. A project_map row is keyed either by a canonicalized git
// remote ("remote:github.com/acme/app") or by an absolute git toplevel path
// ("path:/home/kit/src/app"). The two forms let a pin survive both a folder
// move (the remote is stable) and a dropped/renamed remote (the path is
// stable) — see store.ProjectMapEntry.
const (
	remoteKeyPrefix = "remote:"
	pathKeyPrefix   = "path:"
)

// PinKeys returns the project_map lookup keys for these facts, in preference
// order: the canonical-remote key first (a repo's identity travels with it
// across clones and folder moves), then the toplevel-path key. Either may be
// absent when its fact is; the result is empty when the facts carry neither a
// remote nor a toplevel path (a bare directory, which can only ever derive).
//
// Shared by Resolve (which looks these up to honor a pin) and the pin
// PUT/DELETE handlers (which write/delete under the same keys), so a pin
// created for a project is found by that same project's next handshake.
func PinKeys(f Facts) []string {
	var keys []string
	if canon := CanonicalRemote(f.RemoteURL); canon != "" {
		keys = append(keys, remoteKeyPrefix+canon)
	}
	if f.ToplevelPath != "" {
		keys = append(keys, pathKeyPrefix+f.ToplevelPath)
	}
	return keys
}

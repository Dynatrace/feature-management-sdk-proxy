// Package staticsource implements the offline, single-file config source
// (CONFIG_SOURCE=static). A single flagd JSON document is the sole source of
// truth: it is loaded into memory, split into one served document per flag set,
// and re-loaded whenever the backing file changes.
//
// Flag-set membership follows the flagd "flag set" paradigm: a flag's effective
// flagSetId is its own metadata.flagSetId when present, otherwise the top-level
// (flag-set-level) metadata.flagSetId. Flags with neither belong to no set and
// are never served. The flagSetId value is a full SDK key, matched exactly
// against the key the SDK sends in GET /server/<key>.json.
//
// There is no upstream connectivity in this mode: no CDN fetch, no background
// TTL refresh, and no upstream SSE. Config changes are driven exclusively by
// changes to the backing file, detected by the Watcher's polling loop.
//
// The raw full SDK key is used in-memory as the map key (permitted by design);
// every log, metric, and trace attribute uses sdkkey.PublicID instead.
package staticsource

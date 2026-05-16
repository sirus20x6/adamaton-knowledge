package subunits

import (
	"regexp"
	"strings"
)

// imperativeVerbs is the dictionary of leading verbs that flag a
// procedural subunit. Drawn from a survey of common skill bodies plus
// CLI-style verbs that appear in dev skills. Keep alphabetised; add
// new verbs when extraction misses real procedurals in production.
//
// Match is whole-word, case-insensitive, against the first token of
// the line after bullet markers are stripped. So ``Search Google``
// matches via ``search`` but ``Researching`` does not.
var imperativeVerbs = []string{
	"add", "append", "apply", "assert", "assign", "attach",
	"build", "call", "cancel", "capture", "check", "choose",
	"clean", "clear", "clone", "close", "collect", "commit",
	"compare", "compile", "compute", "configure", "confirm",
	"connect", "consider", "convert", "copy", "create",
	"decode", "decrypt", "dedupe", "dedup", "default", "define",
	"delete", "deploy", "describe", "diff", "disable", "discard",
	"dispatch", "do", "drop", "edit", "emit", "enable", "encode",
	"encrypt", "ensure", "enumerate", "evaluate", "examine",
	"execute", "expand", "export", "extract", "fail", "fetch",
	"filter", "find", "fire", "fix", "flag", "flush", "format",
	"forward", "generate", "get", "group", "guard", "handle",
	"hash", "highlight", "ignore", "import", "include", "index",
	"infer", "init", "initialise", "initialize", "insert",
	"inspect", "install", "invoke", "iterate", "join", "keep",
	"kill", "label", "launch", "limit", "list", "load", "lock",
	"log", "lookup", "loop", "make", "map", "match", "merge",
	"migrate", "monitor", "move", "name", "normalize", "normalise",
	"notify", "open", "output", "parse", "patch", "pick", "ping",
	"plan", "poll", "post", "prefer", "prepare", "preserve",
	"print", "process", "produce", "promote", "propagate", "prove",
	"publish", "pull", "purge", "push", "queue", "query", "raise",
	"read", "reapply", "rebuild", "record", "recover", "redirect",
	"refactor", "refresh", "register", "reject", "release",
	"reload", "rely", "remove", "rename", "render", "replace",
	"reply", "report", "request", "require", "reset", "resolve",
	"respond", "restart", "restore", "retry", "return", "reverse",
	"review", "revoke", "roll", "rollback", "run", "save", "scan",
	"schedule", "score", "search", "select", "send", "serialize",
	"serialise", "serve", "set", "shut", "sign", "skip", "sleep",
	"sort", "split", "stamp", "start", "stop", "store", "stream",
	"strip", "submit", "subscribe", "summarise", "summarize",
	"surface", "sync", "tag", "take", "test", "throttle", "throw",
	"toggle", "trace", "track", "translate", "transmit", "trigger",
	"trim", "truncate", "try", "tune", "unify", "uninstall",
	"unlock", "unset", "update", "upload", "upsert", "use",
	"validate", "verify", "wait", "walk", "warn", "watch", "wire",
	"write", "yield",
}

// reProcedural is built once from imperativeVerbs at init() time. Match
// is anchored at the line start after bullet stripping, with `\b` at
// the verb tail so partial matches (``search`` matching the start of
// ``searching``) don't fire.
var reProcedural = func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)^(?:` + strings.Join(imperativeVerbs, "|") + `)\b`)
}()

// extractProcedural inspects one raw line and returns zero-or-one
// procedural subunit strings (raw, not normalized — Normalize runs in
// Extract). Returns []string for symmetry with the element + constraint
// detectors, both of which can return multiple hits per line.
//
// We accept lines that LOOK procedural even when they lack a bullet
// marker — many skill bodies write steps as continuous paragraphs.
// The verb-at-start filter is conservative enough that prose
// (\"The system will then ...\") doesn't false-positive: \"the\" is
// not in the verb dictionary.
func extractProcedural(line string) []string {
	t := strings.TrimSpace(line)
	if t == "" {
		return nil
	}
	// Skip pure headings — they're document structure, not steps.
	if strings.HasPrefix(t, "#") {
		return nil
	}
	stripped := stripBulletMarker(line)
	stripped = strings.TrimSpace(stripped)
	if stripped == "" {
		return nil
	}
	if !reProcedural.MatchString(stripped) {
		return nil
	}
	return []string{stripped}
}

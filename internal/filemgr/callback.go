package filemgr

import (
	"fmt"
	"strconv"
	"strings"
)

// Action kinds (second segment of the callback).
const (
	KindDir     = "d"       // files:d:<id>[:<page>]
	KindFile    = "f"       // files:f:<id>[:<line>]
	KindCopy    = "copy"    // files:copy:<id>
	KindEdit    = "edit"    // files:edit:<id>
	KindDelAsk  = "delask"  // files:delask:<id>
	KindDelete  = "del"     // files:del:<id>
	KindSend    = "send"    // files:send:<id>    send the file itself
	KindNewDir  = "newdir"  // files:newdir:<id>  prompt: new folder in <id>
	KindNewFile = "newfile" // files:newfile:<id> prompt: new file in <id>
	KindUpload  = "upl"     // files:upl:<id>     prompt: upload files into folder <id>
	KindReplace = "repl"    // files:repl:<id>    prompt: find & replace in file <id>
	KindPatch   = "patch"   // files:patch:<id>   prompt: apply a unified diff
	KindLines   = "lines"   // files:lines:<id>   prompt: +/- and range line edits
	KindGetDoc  = "getdoc"  // files:getdoc:<id>  send the file, prompt for the edited copy
	KindFileNum = "fn"      // files:fn:<id>[:<line>]  file view with line numbers
	KindUndo    = "undo"    // files:undo:<seq>   revert an edit
	KindRename  = "ren"     // files:ren:<id>     prompt: rename <id>
	maxCbNumber = 1<<31 - 1
)

// Action is a parsed `files:*` callback.
type Action struct {
	Kind string
	ID   int
	// Arg is the page (KindDir) or the 1-based start line (KindFile); 0 = first.
	Arg int
}

// ParseCallback parses callback_data such as "files:d:42:3".
func ParseCallback(data string) (Action, error) {
	if !strings.HasPrefix(data, CallbackPrefix) {
		return Action{}, fmt.Errorf("not a files callback")
	}
	parts := strings.Split(strings.TrimPrefix(data, CallbackPrefix), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return Action{}, fmt.Errorf("malformed files callback %q", data)
	}
	a := Action{Kind: parts[0]}
	switch a.Kind {
	case KindDir, KindFile, KindFileNum:
		// optional third segment
	case KindCopy, KindEdit, KindDelAsk, KindDelete, KindSend, KindNewDir, KindNewFile, KindRename, KindUpload, KindReplace, KindPatch, KindUndo, KindLines, KindGetDoc:
		if len(parts) != 2 {
			return Action{}, fmt.Errorf("malformed files callback %q", data)
		}
	default:
		return Action{}, fmt.Errorf("unknown files action %q", a.Kind)
	}
	id, err := parseNum(parts[1])
	if err != nil {
		return Action{}, fmt.Errorf("bad id in %q", data)
	}
	a.ID = id
	if len(parts) == 3 {
		arg, err := parseNum(parts[2])
		if err != nil {
			return Action{}, fmt.Errorf("bad argument in %q", data)
		}
		a.Arg = arg
	}
	return a, nil
}

func parseNum(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > maxCbNumber {
		return 0, fmt.Errorf("out of range")
	}
	return int(n), nil
}

func cbDir(id, page int) string {
	if page > 0 {
		return fmt.Sprintf("files:d:%d:%d", id, page)
	}
	return fmt.Sprintf("files:d:%d", id)
}

func cbFile(id, line int) string {
	if line > 1 {
		return fmt.Sprintf("files:f:%d:%d", id, line)
	}
	return fmt.Sprintf("files:f:%d", id)
}

func cbFileN(id, line int) string {
	if line > 1 {
		return fmt.Sprintf("files:fn:%d:%d", id, line)
	}
	return fmt.Sprintf("files:fn:%d", id)
}

func cbSimple(kind string, id int) string { return fmt.Sprintf("files:%s:%d", kind, id) }

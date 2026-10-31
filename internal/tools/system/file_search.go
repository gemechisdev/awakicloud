package system

// Legacy file_search lives in grep.go as FileSearchTool() (alias of GrepTool).
// This file intentionally has no declarations so older trees cannot double-register
// FileSearchTool / buildRgArgs when both files were present during the grep upgrade.

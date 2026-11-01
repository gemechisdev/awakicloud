package web

// paginateText returns the window [offset, offset+max) of s measured in
// characters (runes), so pages never split a multi-byte character. start/end
// are the window bounds actually returned and total is the page length in
// characters.
func paginateText(s string, offset, max int) (page string, start, end, total int) {
	r := []rune(s)
	total = len(r)
	if max <= 0 {
		max = 50_000
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return "", offset, offset, total
	}
	end = offset + max
	if end > total {
		end = total
	}
	return string(r[offset:end]), offset, end, total
}

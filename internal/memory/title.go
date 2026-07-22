package memory

import (
	"strings"
	"unicode/utf8"
)

const titleMaxRunes = 60

// Title derives a short, human-readable display title from memory content.
// It is deliberately pure: titles are derived at presentation time and never
// participate in stable memory keys or persistence.
func Title(content string) string {
	// Reuse the same frontmatter parser as ingest so title derivation and
	// chunking agree — notably it keeps the whole body when an opening "---"
	// has no closing fence, rather than swallowing it.
	_, body := StripFrontmatter(content)
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}

	inFence := false
	var line string
	for _, candidate := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(candidate)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence || trimmed == "" {
			continue
		}
		line = trimmed
		break
	}
	if line == "" {
		return ""
	}

	line = stripTitleBlockMarkers(line)
	line = stripTitleBoldLabel(line)
	line = stripTitleInlineMarkup(line)
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return ""
	}

	for i := 0; i < len(line); i++ {
		if (line[i] == '.' || line[i] == '!' || line[i] == '?') && (i+1 == len(line) || line[i+1] == ' ') {
			line = strings.TrimSpace(line[:i])
			break
		}
	}
	if line == "" {
		return ""
	}
	if utf8.RuneCountInString(line) <= titleMaxRunes {
		return line
	}

	runes := []rune(line)
	prefix := runes[:titleMaxRunes]
	for i := len(prefix) - 1; i >= 0; i-- {
		if prefix[i] == ' ' {
			return string(prefix[:i]) + "…"
		}
	}
	return string(prefix) + "…"
}

func stripTitleBlockMarkers(line string) string {
	for {
		before := line
		if strings.HasPrefix(line, ">") {
			line = strings.TrimSpace(strings.TrimPrefix(line, ">"))
		}
		if headingLen := titleHeadingMarkerLen(line); headingLen > 0 {
			line = strings.TrimSpace(line[headingLen:])
		}
		if listLen := titleListMarkerLen(line); listLen > 0 {
			line = strings.TrimSpace(line[listLen:])
		}
		if line == before {
			return line
		}
	}
}

func titleHeadingMarkerLen(line string) int {
	i := 0
	for i < len(line) && line[i] == '#' && i < 6 {
		i++
	}
	if i > 0 && i < len(line) && line[i] == ' ' {
		return i + 1
	}
	return 0
}

func titleListMarkerLen(line string) int {
	if len(line) >= 2 && (line[0] == '-' || line[0] == '*' || line[0] == '+') && line[1] == ' ' {
		return 2
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(line) && (line[i] == '.' || line[i] == ')') && line[i+1] == ' ' {
		return i + 2
	}
	return 0
}

func stripTitleBoldLabel(line string) string {
	for _, marker := range []string{"**", "__"} {
		if !strings.HasPrefix(line, marker) {
			continue
		}
		if end := strings.Index(line[len(marker):], marker); end >= 0 {
			end += len(marker)
			if strings.HasSuffix(line[len(marker):end], ":") {
				return strings.TrimSpace(line[end+len(marker):])
			}
		}
	}
	return line
}

func stripTitleInlineMarkup(line string) string {
	var out strings.Builder
	for i := 0; i < len(line); {
		labelStart := i
		if line[i] == '!' && i+1 < len(line) && line[i+1] == '[' {
			labelStart++
		}
		if line[labelStart] == '[' {
			if labelEnd := strings.IndexByte(line[labelStart+1:], ']'); labelEnd >= 0 {
				labelEnd += labelStart + 1
				if labelEnd+1 < len(line) && line[labelEnd+1] == '(' {
					if targetEnd := strings.IndexByte(line[labelEnd+2:], ')'); targetEnd >= 0 {
						out.WriteString(line[labelStart+1 : labelEnd])
						i = labelEnd + 2 + targetEnd + 1
						continue
					}
				}
			}
		}
		if line[i] != '`' && line[i] != '*' && line[i] != '_' {
			out.WriteByte(line[i])
		}
		i++
	}
	return out.String()
}

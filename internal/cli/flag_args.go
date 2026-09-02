package cli

import (
	"fmt"
	"strings"
)

// reorderFlagArgs moves recognized flags ahead of positional arguments so the
// standard library flag parser, which stops at the first positional, accepts
// flags written before or after the positionals. stringFlags are flags that
// consume a following value when not given inline; boolFlags stand alone.
func reorderFlagArgs(args []string, stringFlags map[string]struct{}, boolFlags map[string]struct{}) ([]string, error) {
	flags := []string{}
	positionals := []string{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		name, hasInlineValue := splitFlagName(arg)
		if _, ok := boolFlags[name]; ok {
			flags = append(flags, arg)
			continue
		}
		if _, ok := stringFlags[name]; ok {
			flags = append(flags, arg)
			if !hasInlineValue {
				if index+1 >= len(args) {
					return nil, fmt.Errorf("flag needs an argument: %s", arg)
				}
				index++
				flags = append(flags, args[index])
			}
			continue
		}
		flags = append(flags, arg)
	}
	return append(flags, positionals...), nil
}

func splitFlagName(arg string) (string, bool) {
	arg = strings.TrimLeft(arg, "-")
	name, value, hasInlineValue := strings.Cut(arg, "=")
	_ = value
	return name, hasInlineValue
}

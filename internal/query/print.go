package query

import (
	"strconv"
	"strings"
)

// Print writes a tree in its canonical form: one space between tokens,
// keywords for the tags the dictionary names, parentheses only where the
// tree needs them, "=" for a folded equality and "==" for an exact one.
// Parse of what Print writes gives the same tree, for every tree Check
// accepts.
func Print(n Node) string {
	var b strings.Builder
	printNode(&b, n)
	return b.String()
}

// printNode writes n.
func printNode(b *strings.Builder, n Node) {
	switch n := n.(type) {
	case *Or:
		for i, c := range n.Children {
			if i > 0 {
				b.WriteString(" or ")
			}
			_, nested := c.(*Or)
			printChild(b, c, nested)
		}
	case *And:
		for i, c := range n.Children {
			if i > 0 {
				b.WriteString(" and ")
			}
			_, or := c.(*Or)
			_, and := c.(*And)
			printChild(b, c, or || and)
		}
	case *Not:
		b.WriteString("not ")
		_, or := n.Child.(*Or)
		_, and := n.Child.(*And)
		printChild(b, n.Child, or || and)
	case *Has:
		if n.Every {
			b.WriteString("every ")
		} else {
			b.WriteString("has ")
		}
		b.WriteString(string(n.Level))
		b.WriteString(" (")
		printNode(b, n.Child)
		b.WriteString(")")
	case *Leaf:
		printLeaf(b, n)
	}
}

// printChild writes c, in parentheses when group.
func printChild(b *strings.Builder, c Node, group bool) {
	if group {
		b.WriteString("(")
	}
	printNode(b, c)
	if group {
		b.WriteString(")")
	}
}

// printLeaf writes one leaf.
func printLeaf(b *strings.Builder, l *Leaf) {
	b.WriteString(FieldString(l.Field))
	b.WriteString(" ")
	text := len(l.Values) > 0 && l.Values[0].Kind == KindText
	switch l.Op {
	case OpEqual:
		if text && !l.Fold {
			b.WriteString("==")
		} else {
			b.WriteString("=")
		}
	case OpPresent, OpAbsent:
		b.WriteString(l.Op.String())
		return
	default:
		if l.Fold && l.Op != OpContains && l.Op != OpWords {
			b.WriteString("nocase ")
		}
		b.WriteString(l.Op.String())
	}
	b.WriteString(" ")
	switch l.Op {
	case OpIn:
		b.WriteString("(")
		for i, v := range l.Values {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(ValueString(v))
		}
		b.WriteString(")")
	case OpBetween:
		b.WriteString(ValueString(l.Values[0]))
		b.WriteString(" and ")
		b.WriteString(ValueString(l.Values[1]))
	default:
		b.WriteString(ValueString(l.Values[0]))
	}
}

// FieldString writes a field as Parse reads it: a catalog field's name, or
// the steps of an attribute's path joined by ".", each step the keyword
// the dictionary gives its tag, else (GGGG,EEEE), or (GGGG,"CREATOR",EE)
// for a private tag by creator, then "[index]" when it has one. It is the
// field's one spelling, which the registry keys its columns by.
func FieldString(f Field) string {
	if f.Catalog != "" {
		return f.Catalog
	}
	var b strings.Builder
	for i, s := range f.Path {
		if i > 0 {
			b.WriteString(".")
		}
		b.WriteString(stepString(s))
	}
	if f.Indexed {
		b.WriteString("[" + strconv.Itoa(f.Index) + "]")
	}
	return b.String()
}

// stepString writes one step.
func stepString(s Step) string {
	group := hex4(s.Tag >> 16)
	if s.Creator != "" {
		return "(" + group + "," + quote(s.Creator) + "," + hex4(s.Tag & 0xFF)[2:] + ")"
	}
	if k, ok := tagKeyword(s.Tag); ok {
		return k
	}
	return "(" + group + "," + hex4(s.Tag&0xFFFF) + ")"
}

// hex4 writes v as four upper-case hexadecimal digits.
func hex4(v uint32) string {
	const digits = "0123456789ABCDEF"
	return string([]byte{digits[v>>12&0xF], digits[v>>8&0xF], digits[v>>4&0xF], digits[v&0xF]})
}

// ValueString writes a value as Parse reads it.
func ValueString(v Value) string {
	switch v.Kind {
	case KindText:
		return quote(v.Text)
	case KindNumber:
		return strconv.FormatFloat(v.Number, 'g', -1, 64)
	case KindDate:
		return dateString(v.Text)
	case KindTime:
		return timeString(v.Text)
	case KindDateTime:
		if len(v.Text) == 14 {
			return dateString(v.Text[:8]) + "T" + timeString(v.Text[8:])
		}
	case KindRelative:
		if v.Relative == 0 && v.Unit == 'd' {
			return "today"
		}
		return "-" + strconv.Itoa(v.Relative) + string(v.Unit)
	}
	return v.Text
}

// dateString writes YYYYMMDD as YYYY-MM-DD.
func dateString(s string) string {
	if len(s) != 8 {
		return s
	}
	return s[:4] + "-" + s[4:6] + "-" + s[6:]
}

// timeString writes HHMMSS as HH:MM:SS.
func timeString(s string) string {
	if len(s) != 6 {
		return s
	}
	return s[:2] + ":" + s[2:4] + ":" + s[4:]
}

// quote writes s as a string Parse reads: in double quotes, with `"` and
// `\` escaped by a backslash and nothing else changed.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

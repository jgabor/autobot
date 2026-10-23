// Package memory stores what autobot learns: one shared file and one file per
// task, kept as Markdown bullet lists with stable IDs and versioned in git.
package memory

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Standard sections, in the order new files list them.
var Sections = []string{"Locations", "Procedures", "Gotchas", "Insights", "Follow-ups"}

// Item is one remembered fact.
type Item struct {
	ID   string
	Text string
}

// Section is a heading and its items.
type Section struct {
	Name  string
	Items []Item
}

// Doc is one memory file.
type Doc struct {
	Title    string
	Prefix   string // "m" for task memory, "s" for shared memory
	Sections []Section
}

// New returns an empty memory file with the standard sections.
func New(title, prefix string) *Doc {
	d := &Doc{Title: title, Prefix: prefix}
	for _, s := range Sections {
		d.Sections = append(d.Sections, Section{Name: s})
	}
	return d
}

var itemRe = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+(?:\[([a-z]+-\d+)\]\s*)?(.*)$`)

// Parse reads a memory file. It is forgiving because people edit these
// files by hand: lines without an ID get one, and any non-empty line under a
// heading becomes an item, so nothing written by hand is dropped.
func Parse(text, title, prefix string) *Doc {
	d := &Doc{Title: title, Prefix: prefix}
	var cur *Section
	var pending []*Item
	used := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case strings.HasPrefix(trimmed, "# "):
			d.Title = strings.TrimSpace(trimmed[2:])
			continue
		case strings.HasPrefix(trimmed, "## "):
			d.Sections = append(d.Sections, Section{Name: strings.TrimSpace(trimmed[3:])})
			cur = &d.Sections[len(d.Sections)-1]
			continue
		}
		if cur == nil {
			d.Sections = append(d.Sections, Section{Name: "Notes"})
			cur = &d.Sections[len(d.Sections)-1]
		}
		item := Item{Text: trimmed}
		if m := itemRe.FindStringSubmatch(line); m != nil {
			item.Text = strings.TrimSpace(m[2])
			if m[1] != "" && strings.HasPrefix(m[1], prefix+"-") && !used[m[1]] {
				item.ID = m[1]
				used[m[1]] = true
			}
		}
		if item.Text == "" {
			continue
		}
		cur.Items = append(cur.Items, item)
	}
	for _, s := range Sections {
		if d.sectionIndex(s) < 0 {
			d.Sections = append(d.Sections, Section{Name: s})
		}
	}
	// Assign IDs after parsing so new IDs never collide with existing ones.
	for si := range d.Sections {
		for ii := range d.Sections[si].Items {
			if d.Sections[si].Items[ii].ID == "" {
				pending = append(pending, &d.Sections[si].Items[ii])
			}
		}
	}
	for _, it := range pending {
		it.ID = d.nextID()
	}
	return d
}

// Render writes the file back as Markdown.
func (d *Doc) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", d.Title)
	for _, s := range d.Sections {
		fmt.Fprintf(&b, "\n## %s\n", s.Name)
		if len(s.Items) > 0 {
			b.WriteString("\n")
		}
		for _, it := range s.Items {
			if it.ID == "" {
				fmt.Fprintf(&b, "- %s\n", it.Text)
			} else {
				fmt.Fprintf(&b, "- [%s] %s\n", it.ID, it.Text)
			}
		}
	}
	return b.String()
}

// Len is the number of items.
func (d *Doc) Len() int {
	n := 0
	for _, s := range d.Sections {
		n += len(s.Items)
	}
	return n
}

func (d *Doc) clone() *Doc {
	c := &Doc{Title: d.Title, Prefix: d.Prefix, Sections: make([]Section, len(d.Sections))}
	for i, s := range d.Sections {
		c.Sections[i] = Section{Name: s.Name, Items: append([]Item(nil), s.Items...)}
	}
	return c
}

func (d *Doc) sectionIndex(name string) int {
	norm := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, "-", ""), " ", ""))
	}
	for i, s := range d.Sections {
		if norm(s.Name) == norm(name) {
			return i
		}
	}
	return -1
}

func (d *Doc) find(id string) (int, int, bool) {
	for si, s := range d.Sections {
		for ii, it := range s.Items {
			if it.ID == id {
				return si, ii, true
			}
		}
	}
	return 0, 0, false
}

func (d *Doc) nextID() string {
	top := 0
	for _, s := range d.Sections {
		for _, it := range s.Items {
			if n, err := strconv.Atoi(strings.TrimPrefix(it.ID, d.Prefix+"-")); err == nil && n > top {
				top = n
			}
		}
	}
	return fmt.Sprintf("%s-%d", d.Prefix, top+1)
}

// Op is one change reflection asks for.
type Op struct {
	Op      string `json:"op"`
	Scope   string `json:"scope,omitempty"`
	Section string `json:"section,omitempty"`
	ID      string `json:"id,omitempty"`
	Text    string `json:"text,omitempty"`
}

// MaxItemText limits one memory item.
const MaxItemText = 500

// Apply validates every op against copies of the two files and returns the
// changed copies. Nothing is changed if any op is invalid.
func Apply(task, shared *Doc, ops []Op) (*Doc, *Doc, error) {
	t, s := task.clone(), shared.clone()
	pick := func(scope, id string) (*Doc, error) {
		switch {
		case id != "" && strings.HasPrefix(id, s.Prefix+"-"):
			return s, nil
		case id != "" && strings.HasPrefix(id, t.Prefix+"-"):
			return t, nil
		case id != "":
			return nil, fmt.Errorf("unknown id %q", id)
		case scope == "shared":
			return s, nil
		case scope == "" || scope == "task":
			return t, nil
		}
		return nil, fmt.Errorf("scope must be task or shared, got %q", scope)
	}
	for i, op := range ops {
		fail := func(err error) (*Doc, *Doc, error) {
			return nil, nil, fmt.Errorf("change %d (%s): %w", i+1, op.Op, err)
		}
		text := strings.Join(strings.Fields(op.Text), " ")
		if len(text) > MaxItemText {
			return fail(fmt.Errorf("text is longer than %d characters", MaxItemText))
		}
		switch op.Op {
		case "add":
			if op.ID != "" {
				return fail(errors.New("add must not set id"))
			}
			if text == "" {
				return fail(errors.New("text is required"))
			}
			d, err := pick(op.Scope, "")
			if err != nil {
				return fail(err)
			}
			si := d.sectionIndex(op.Section)
			if si < 0 {
				return fail(fmt.Errorf("unknown section %q; use one of %s", op.Section, strings.Join(Sections, ", ")))
			}
			d.Sections[si].Items = append(d.Sections[si].Items, Item{ID: d.nextID(), Text: text})
		case "update":
			if text == "" {
				return fail(errors.New("text is required"))
			}
			d, err := pick("", op.ID)
			if err != nil {
				return fail(err)
			}
			si, ii, ok := d.find(op.ID)
			if !ok {
				return fail(fmt.Errorf("unknown id %q", op.ID))
			}
			d.Sections[si].Items[ii].Text = text
		case "remove":
			d, err := pick("", op.ID)
			if err != nil {
				return fail(err)
			}
			si, ii, ok := d.find(op.ID)
			if !ok {
				return fail(fmt.Errorf("unknown id %q", op.ID))
			}
			items := d.Sections[si].Items
			d.Sections[si].Items = append(items[:ii:ii], items[ii+1:]...)
		default:
			return fail(errors.New(`op must be "add", "update" or "remove"`))
		}
	}
	return t, s, nil
}

package manifest

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strings"
)

type element struct {
	name     xml.Name
	attrs    []xml.Attr
	text     string
	children []*element
}

func readXML(body []byte) (*element, error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	var root *element
	var stack []*element
	count := 0
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := token.(type) {
		case xml.Directive:
			return nil, errors.New("XML directives unsupported")
		case xml.StartElement:
			count++
			if count > 10000 || len(stack) > 32 {
				return nil, errors.New("XML limit")
			}
			n := &element{name: t.Name}
			for _, a := range t.Attr {
				if a.Name.Local != "xmlns" && a.Name.Space != "xmlns" {
					n.attrs = append(n.attrs, a)
				}
			}
			for _, a := range t.Attr {
				if a.Name.Local == "href" || a.Name.Local == "base" {
					return nil, errors.New("external XML reference")
				}
			}
			if len(stack) == 0 {
				if root != nil {
					return nil, errors.New("multiple roots")
				}
				root = n
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, errors.New("invalid XML")
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil || root.name.Local != "MPD" {
		return nil, errors.New("not DASH")
	}
	return root, nil
}

func (n *element) attr(name string) string {
	for _, a := range n.attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
func (n *element) write(e *xml.Encoder) error {
	start := xml.StartElement{Name: n.name, Attr: n.attrs}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.CharData(n.text)); err != nil {
		return err
	}
	for _, c := range n.children {
		if err := c.write(e); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

func clone(n *element) *element {
	if n == nil {
		return nil
	}
	out := &element{name: n.name, attrs: append([]xml.Attr(nil), n.attrs...), text: n.text}
	for _, c := range n.children {
		out.children = append(out.children, clone(c))
	}
	return out
}

func inherit(parent, child *element) *element {
	if child == nil {
		return clone(parent)
	}
	if parent == nil || parent.name.Local != child.name.Local {
		return clone(child)
	}
	out := clone(parent)
	for _, a := range child.attrs {
		replaced := false
		for i, old := range out.attrs {
			if old.Name.Local == a.Name.Local {
				out.attrs[i] = a
				replaced = true
				break
			}
		}
		if !replaced {
			out.attrs = append(out.attrs, a)
		}
	}
	if len(child.children) > 0 {
		out.children = clone(child).children
	}
	return out
}

var templateVariable = regexp.MustCompile(`\$(Number|Time)(%0[1-9][0-9]?d)?\$`)

func (p *Proxy) dash(base string, body []byte, depth int) ([]byte, error) {
	root, err := readXML(body)
	if err != nil {
		return nil, err
	}
	var visit func(*element, string, *element) error
	visit = func(n *element, origin string, inherited *element) error {
		var ownTemplate *element
		var kept []*element
		bases := 0
		for _, c := range n.children {
			switch c.name.Local {
			case "BaseURL":
				bases++
				if bases > 1 {
					return errors.New("multiple DASH origins")
				}
				origin, err = resolve(origin, strings.TrimSpace(c.text))
				if err != nil {
					return err
				}
			case "SegmentTemplate", "SegmentList", "SegmentBase":
				ownTemplate = c
			case "ContentProtection":
				return errors.New("encrypted DASH unsupported")
			case "Location", "UTCTiming", "PatchLocation": // No independent network/time fetches.
			default:
				kept = append(kept, c)
			}
		}
		n.children = kept
		merged := inherit(inherited, ownTemplate)
		if n.name.Local == "Representation" {
			if merged != nil {
				if merged.name.Local == "SegmentList" {
					if root.attr("type") == "dynamic" {
						return errors.New("dynamic DASH SegmentList unsupported")
					}
					// FFmpeg indexes explicit lists from zero, including when the
					// MPD carries a nonzero startNumber intended for segment numbering.
					for i, a := range merged.attrs {
						if a.Name.Local == "startNumber" {
							merged.attrs[i].Value = "0"
						}
					}
				}
				n.children = append(n.children, merged)
			}
			mapped, err := p.register(origin, "segment", depth+1, nil)
			if err != nil {
				return err
			}
			n.children = append([]*element{{name: xml.Name{Local: "BaseURL"}, text: mapped}}, n.children...)
			for _, c := range n.children {
				if c.name.Local != "BaseURL" {
					if err := p.dashResources(c, origin, n.attr("id"), n.attr("bandwidth"), depth); err != nil {
						return err
					}
				}
			}
			return nil
		}
		for _, c := range n.children {
			if err := visit(c, origin, merged); err != nil {
				return err
			}
		}
		return nil
	}
	if err = visit(root, base, nil); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	e := xml.NewEncoder(&out)
	if err = root.write(e); err != nil {
		return nil, err
	}
	if err = e.Flush(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (p *Proxy) dashResources(n *element, base, id, bandwidth string, depth int) error {
	if n.name.Local == "ContentProtection" || n.name.Local == "BaseURL" {
		return errors.New("encrypted DASH unsupported")
	}
	for i, a := range n.attrs {
		switch a.Name.Local {
		case "media", "initialization", "sourceURL", "index":
			raw := strings.ReplaceAll(strings.ReplaceAll(a.Value, "$RepresentationID$", id), "$Bandwidth$", bandwidth)
			variables := templateVariable.FindAllString(raw, -1)
			if len(variables) > 2 || strings.Contains(templateVariable.ReplaceAllString(raw, ""), "$") {
				return errors.New("unsupported DASH template")
			}
			safe := raw
			for j, v := range variables {
				safe = strings.ReplaceAll(safe, v, "ZOMBIEVAR"+string(rune('A'+j)))
			}
			target, err := resolve(base, safe)
			if err != nil {
				return err
			}
			for j, v := range variables {
				target = strings.ReplaceAll(target, "ZOMBIEVAR"+string(rune('A'+j)), v)
			}
			// URL escaping changes % in numeric formatting; restore only recognized placeholders.
			for _, v := range variables {
				target = strings.ReplaceAll(target, strings.ReplaceAll(v, "%", "%25"), v)
			}
			mapped, err := p.register(target, "segment", depth+1, variables)
			if err != nil {
				return err
			}
			n.attrs[i].Value = mapped
		case "bitstreamSwitching":
			if a.Value != "true" && a.Value != "false" {
				return errors.New("unsupported DASH resource")
			}
		}
	}
	for _, c := range n.children {
		if err := p.dashResources(c, base, id, bandwidth, depth); err != nil {
			return err
		}
	}
	return nil
}

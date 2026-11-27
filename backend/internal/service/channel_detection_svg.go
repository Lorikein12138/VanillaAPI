package service

import (
	"encoding/xml"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const (
	detectionSVGNamespace = "http://www.w3.org/2000/svg"
	detectionSVGMaxBytes  = 64 << 10
)

var (
	detectionSVGTags = map[string]bool{
		"svg": true, "g": true, "path": true, "rect": true, "circle": true,
		"ellipse": true, "line": true, "polyline": true, "polygon": true,
		"defs": true, "linearGradient": true, "radialGradient": true, "stop": true,
	}
	detectionSVGPaints = regexp.MustCompile(`^(?:[a-zA-Z]{1,24}|#[0-9a-fA-F]{3,8}|url\(#[a-zA-Z_][a-zA-Z_0-9-]{0,63}\)|(?:rgb|rgba|hsl|hsla)\([0-9.,% +\-]+\))$`)
	detectionSVGID     = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z_0-9-]{0,63}$`)
	detectionSVGNum    = regexp.MustCompile(detectionSVGNumber)
	detectionSVGSingle = regexp.MustCompile(`^` + detectionSVGNumber + `%?$`)
	detectionSVGList   = regexp.MustCompile(`^[0-9.,eE%+\- \t\r\n]+$`)
	detectionSVGPath   = regexp.MustCompile(`^[MmZzLlHhVvCcSsQqTtAaEe0-9.,+\- \t\r\n]+$`)
	detectionSVGTrans  = regexp.MustCompile(`^(?:\s*(?:matrix|translate|scale|rotate|skewX|skewY)\([0-9.,+\- \t\r\n]+\)\s*,?)+$`)
)

const detectionSVGNumber = `[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`

// sanitizeDetectionSVG rebuilds a small drawing vocabulary. The result is
// transported as data and rendered only through an img element.
func sanitizeDetectionSVG(input string) string {
	input = strings.TrimSpace(input)
	if len(input) > detectionSVGMaxBytes {
		return ""
	}
	for _, fence := range []string{"```svg\n", "```xml\n", "```\n", "```svg\r\n", "```xml\r\n", "```\r\n"} {
		if strings.HasPrefix(input, fence) && strings.HasSuffix(input, "```") {
			input = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(input, fence), "```"))
			break
		}
	}
	in := xml.NewDecoder(strings.NewReader(input))
	var output strings.Builder
	out := xml.NewEncoder(&output)
	depth, nodes := 0, 0
	for {
		token, err := in.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ""
		}
		switch elem := token.(type) {
		case xml.StartElement:
			nodes++
			if depth >= 16 || nodes > 500 || (depth == 0 && (nodes != 1 || elem.Name.Local != "svg")) ||
				(depth != 0 && elem.Name.Local == "svg") || !detectionSVGTags[elem.Name.Local] ||
				(elem.Name.Space != "" && elem.Name.Space != detectionSVGNamespace) {
				return ""
			}
			depth++
			attributes, ok := detectionSVGAttributes(elem.Attr, depth == 1)
			if !ok || out.EncodeToken(xml.StartElement{Name: xml.Name{Local: elem.Name.Local}, Attr: attributes}) != nil {
				return ""
			}
		case xml.EndElement:
			depth--
			if out.EncodeToken(xml.EndElement{Name: xml.Name{Local: elem.Name.Local}}) != nil {
				return ""
			}
		case xml.CharData:
			if strings.TrimSpace(string(elem)) != "" {
				return ""
			}
		case xml.Comment:
			// Comments carry no drawing data.
		case xml.ProcInst:
			if nodes != 0 || elem.Target != "xml" {
				return ""
			}
		default:
			return ""
		}
	}
	if nodes < 2 || depth != 0 || out.Flush() != nil || output.Len() > detectionSVGMaxBytes {
		return ""
	}
	return output.String()
}

func detectionSVGAttributes(input []xml.Attr, root bool) ([]xml.Attr, bool) {
	if len(input) > 40 {
		return nil, false
	}
	out := make([]xml.Attr, 0, len(input)+2)
	seen := make(map[string]bool, len(input))
	for _, attr := range input {
		key, value := attr.Name.Local, strings.TrimSpace(attr.Value)
		if attr.Name.Space != "" || seen[key] {
			return nil, false
		}
		seen[key] = true
		if key == "xmlns" && root && value == detectionSVGNamespace {
			continue
		}
		if !validDetectionSVGAttribute(key, value) {
			return nil, false
		}
		out = append(out, xml.Attr{Name: xml.Name{Local: key}, Value: value})
	}
	if root {
		out = append(out, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: detectionSVGNamespace})
		if !seen["viewBox"] {
			out = append(out, xml.Attr{Name: xml.Name{Local: "viewBox"}, Value: "0 0 480 360"})
		}
	}
	return out, true
}

func validDetectionSVGAttribute(key, value string) bool {
	switch key {
	case "fill", "stroke", "stop-color", "color":
		return detectionSVGPaints.MatchString(value) && len(value) <= 80
	case "id":
		return detectionSVGID.MatchString(value)
	case "d":
		return detectionSVGPath.MatchString(value) && detectionSVGNumbersBounded(value)
	case "transform", "gradientTransform":
		return len(value) <= 300 && detectionSVGTrans.MatchString(value) && detectionSVGNumbersBounded(value)
	case "viewBox":
		values := strings.Fields(strings.ReplaceAll(value, ",", " "))
		if len(values) != 4 {
			return false
		}
		for i, part := range values {
			number, err := strconv.ParseFloat(part, 64)
			if err != nil || detectionSVGNum.FindString(part) != part || number < -10000 || number > 10000 || (i >= 2 && number < 1) {
				return false
			}
		}
		return true
	case "points", "stroke-dasharray":
		return detectionSVGList.MatchString(value) && detectionSVGNumbersBounded(value)
	case "x", "y", "x1", "x2", "y1", "y2", "width", "height", "rx", "ry",
		"cx", "cy", "fx", "fy", "r", "fr", "stroke-width", "stroke-miterlimit",
		"stroke-dashoffset", "opacity", "fill-opacity", "stroke-opacity", "stop-opacity", "offset":
		if !detectionSVGSingle.MatchString(value) {
			return false
		}
		number, err := strconv.ParseFloat(strings.TrimSuffix(value, "%"), 64)
		if err != nil || number < -10000 || number > 10000 {
			return false
		}
		switch key {
		case "width", "height", "r", "rx", "ry", "fr", "stroke-width":
			return number >= 0
		case "stroke-miterlimit":
			return !strings.HasSuffix(value, "%") && number >= 1
		default:
			return true
		}
	case "fill-rule":
		return value == "nonzero" || value == "evenodd"
	case "stroke-linecap":
		return value == "butt" || value == "round" || value == "square"
	case "stroke-linejoin":
		return value == "miter" || value == "round" || value == "bevel"
	case "gradientUnits":
		return value == "userSpaceOnUse" || value == "objectBoundingBox"
	case "spreadMethod":
		return value == "pad" || value == "reflect" || value == "repeat"
	default:
		return false
	}
}

func detectionSVGNumbersBounded(value string) bool {
	numbers := detectionSVGNum.FindAllString(value, 4097)
	if len(numbers) == 0 || len(numbers) > 4096 {
		return false
	}
	for _, part := range numbers {
		number, err := strconv.ParseFloat(part, 64)
		if err != nil || number < -10000 || number > 10000 {
			return false
		}
	}
	return true
}

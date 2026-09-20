package providers

import (
	"bytes"
	"encoding/xml"
	"io"
	"sort"
	"time"
	"zombiebox.local/gateway/internal/domain"
)

// ParseXMLTV keeps only current/upcoming programmes and a bounded guide per channel.
// Decoding is streaming; external entities are not resolved by encoding/xml.
func ParseXMLTV(body []byte, now time.Time) (map[string][]domain.Programme, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	result := map[string][]domain.Programme{}
	count := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "programme" {
			continue
		}
		var p struct {
			Channel     string `xml:"channel,attr"`
			Start       string `xml:"start,attr"`
			Stop        string `xml:"stop,attr"`
			Title       string `xml:"title"`
			Description string `xml:"desc"`
		}
		if err = decoder.DecodeElement(&p, &start); err != nil {
			return nil, err
		}
		count++
		if count > 20000 {
			return result, nil
		}
		begin, e1 := time.Parse("20060102150405 -0700", p.Start)
		end, e2 := time.Parse("20060102150405 -0700", p.Stop)
		if e1 != nil || e2 != nil || !end.After(now) || !end.After(begin) || p.Channel == "" || p.Title == "" || begin.After(now.Add(24*time.Hour)) {
			continue
		}
		// Descriptions are deliberately excluded from the channel row payload.
		result[p.Channel] = append(result[p.Channel], domain.Programme{Title: p.Title, Start: begin.Unix(), End: end.Unix()})
		sort.Slice(result[p.Channel], func(i, j int) bool { return result[p.Channel][i].Start < result[p.Channel][j].Start })
		if len(result[p.Channel]) > 8 {
			result[p.Channel] = result[p.Channel][:8]
		}
	}
}

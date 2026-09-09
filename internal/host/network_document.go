package host

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// Keep XML additions fail-closed. The semantic projection alone ignores unknown
// XML, which could otherwise silently change DNS, routing or NAT behavior.
type networkXMLNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr       `xml:",any,attr"`
	Children []networkXMLNode `xml:",any"`
	Text     string           `xml:",chardata"`
}

func (p *networkPolicy) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var tree networkXMLNode
	if e := d.DecodeElement(&tree, &start); e != nil {
		return e
	}
	if e := validateNetworkXML(tree, "network"); e != nil {
		return fmt.Errorf("libvirt network security policy drift: %w", e)
	}
	b, e := xml.Marshal(tree)
	if e != nil {
		return e
	}
	type plainPolicy networkPolicy
	var value plainPolicy
	if e = xml.Unmarshal(b, &value); e != nil {
		return e
	}
	*p = networkPolicy(value)
	return nil
}

func validateNetworkXML(n networkXMLNode, path string) error {
	attrs := map[string]string{}
	permitted := map[string]string{
		"network":      "ipv6 connections",
		"network/name": "", "network/uuid": "", "network/metadata": "",
		"network/metadata/owner": "cluster", "network/forward": "mode dev",
		"network/forward/nat": "", "network/forward/nat/port": "start end",
		"network/bridge": "name stp delay", "network/mac": "address",
		"network/domain": "name localOnly register", "network/port": "isolated",
		"network/ip": "family address netmask prefix", "network/ip/dhcp": "",
		"network/ip/dhcp/range": "start end",
	}
	allow, ok := permitted[path]
	if !ok {
		return fmt.Errorf("unsupported element %s", path)
	}
	wantNS := ""
	if path == "network/metadata/owner" {
		wantNS = "urn:runnerloom:network"
	}
	if n.XMLName.Space != wantNS {
		return fmt.Errorf("unexpected namespace at %s", path)
	}
	for _, a := range n.Attrs {
		if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
			continue
		}
		if a.Name.Space != "" || !strings.Contains(" "+allow+" ", " "+a.Name.Local+" ") {
			return fmt.Errorf("unsupported attribute %s/%s", path, a.Name.Local)
		}
		if _, dup := attrs[a.Name.Local]; dup {
			return fmt.Errorf("duplicate attribute at %s", path)
		}
		attrs[a.Name.Local] = a.Value
	}
	if path != "network/name" && path != "network/uuid" && strings.TrimSpace(n.Text) != "" {
		return fmt.Errorf("unexpected text at %s", path)
	}
	switch path {
	case "network":
		if v, ok := attrs["connections"]; ok {
			if _, e := strconv.ParseUint(v, 10, 64); e != nil {
				return fmt.Errorf("invalid libvirt connection counter")
			}
		}
	case "network/domain":
		if attrs["name"] != "runnerloom.invalid" || attrs["localOnly"] != "yes" || (attrs["register"] != "" && attrs["register"] != "no") {
			return fmt.Errorf("unexpected DNS domain policy")
		}
	case "network/forward/nat/port":
		// libvirt materializes this default in net-dumpxml. Other translations must
		// not become implicitly approved merely because the UUID is unchanged.
		if attrs["start"] != "1024" || attrs["end"] != "65535" {
			return fmt.Errorf("unexpected NAT source-port policy")
		}
	}
	count := map[string]int{}
	for _, child := range n.Children {
		count[child.XMLName.Local]++
		if count[child.XMLName.Local] > 1 {
			return fmt.Errorf("duplicate element at %s/%s", path, child.XMLName.Local)
		}
		if e := validateNetworkXML(child, path+"/"+child.XMLName.Local); e != nil {
			return e
		}
	}
	required := map[string][]string{
		"network":          {"name", "metadata", "forward", "bridge", "domain", "port", "ip"},
		"network/metadata": {"owner"}, "network/ip": {"dhcp"}, "network/ip/dhcp": {"range"},
	}
	for _, name := range required[path] {
		if count[name] != 1 {
			return fmt.Errorf("missing %s/%s", path, name)
		}
	}
	return nil
}

package porkbun

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/StackExchange/dnscontrol/v3/models"
	"github.com/StackExchange/dnscontrol/v3/pkg/diff"
	"github.com/StackExchange/dnscontrol/v3/pkg/diff2"
	"github.com/StackExchange/dnscontrol/v3/pkg/printer"
	"github.com/StackExchange/dnscontrol/v3/providers"
)

const (
	minimumTTL = 600
)

// https://kb.porkbun.com/article/63-how-to-switch-to-porkbuns-nameservers
var defaultNS = []string{
	"curitiba.ns.porkbun.com",
	"fortaleza.ns.porkbun.com",
	"maceio.ns.porkbun.com",
	"salvador.ns.porkbun.com",
}

// NewPorkbun creates the provider.
func NewPorkbun(m map[string]string, metadata json.RawMessage) (providers.DNSServiceProvider, error) {
	c := &porkbunProvider{}

	c.apiKey, c.secretKey = m["api_key"], m["secret_key"]

	if c.apiKey == "" || c.secretKey == "" {
		return nil, fmt.Errorf("missing porkbun api_key or secret_key")
	}

	// Validate authentication
	if err := c.ping(); err != nil {
		return nil, err
	}

	return c, nil
}

var features = providers.DocumentationNotes{
	providers.CanAutoDNSSEC:          providers.Cannot(),
	providers.CanGetZones:            providers.Can(),
	providers.CanUseAlias:            providers.Can(),
	providers.CanUseCAA:              providers.Unimplemented(), // CAA record for base domain is pinning to a fixed set once configure
	providers.CanUseDS:               providers.Cannot(),
	providers.CanUseDSForChildren:    providers.Cannot(),
	providers.CanUseNAPTR:            providers.Cannot(),
	providers.CanUsePTR:              providers.Cannot(),
	providers.CanUseSOA:              providers.Cannot(),
	providers.CanUseSRV:              providers.Can(),
	providers.CanUseSSHFP:            providers.Cannot(),
	providers.CanUseTLSA:             providers.Can(),
	providers.DocCreateDomains:       providers.Cannot(),
	providers.DocDualHost:            providers.Cannot(),
	providers.DocOfficiallySupported: providers.Cannot(),
}

func init() {
	fns := providers.DspFuncs{
		Initializer:   NewPorkbun,
		RecordAuditor: AuditRecords,
	}
	providers.RegisterDomainServiceProviderType("PORKBUN", fns, features)
}

// GetNameservers returns the nameservers for a domain.
func (c *porkbunProvider) GetNameservers(domain string) ([]*models.Nameserver, error) {
	return models.ToNameservers(defaultNS)
}

// GetDomainCorrections returns the corrections for a domain.
func (c *porkbunProvider) GetDomainCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
	dc, err := dc.Copy()
	if err != nil {
		return nil, err
	}

	dc.Punycode()

	existingRecords, err := c.GetZoneRecords(dc.Name)
	if err != nil {
		return nil, err
	}

	// Block changes to NS records for base domain
	checkNSModifications(dc)

	// Normalize
	models.PostProcessRecords(existingRecords)

	// Make sure TTL larger than the minimum TTL
	for _, record := range dc.Records {
		record.TTL = fixTTL(record.TTL)
	}

	var corrections []*models.Correction
	if !diff2.EnableDiff2 {

		differ := diff.New(dc)
		_, create, del, modify, err := differ.IncrementalDiff(existingRecords)
		if err != nil {
			return nil, err
		}

		// Deletes first so changing type works etc.
		for _, m := range del {
			id := m.Existing.Original.(*domainRecord).ID
			corr := &models.Correction{
				Msg: fmt.Sprintf("%s, porkbun ID: %s", m.String(), id),
				F: func() error {
					return c.deleteRecord(dc.Name, id)
				},
			}
			corrections = append(corrections, corr)
		}

		for _, m := range create {
			req, err := toReq(m.Desired)
			if err != nil {
				return nil, err
			}

			corr := &models.Correction{
				Msg: m.String(),
				F: func() error {
					return c.createRecord(dc.Name, req)
				},
			}
			corrections = append(corrections, corr)
		}

		for _, m := range modify {
			id := m.Existing.Original.(*domainRecord).ID
			req, err := toReq(m.Desired)
			if err != nil {
				return nil, err
			}

			corr := &models.Correction{
				Msg: fmt.Sprintf("%s, porkbun ID: %s", m.String(), id),
				F: func() error {
					return c.modifyRecord(dc.Name, id, req)
				},
			}
			corrections = append(corrections, corr)
		}

		return corrections, nil
	}

	changes, err := diff2.ByRecord(existingRecords, dc, nil)
	if err != nil {
		return nil, err
	}
	for _, change := range changes {
		var corr *models.Correction
		switch change.Type {
		case diff2.CREATE:
			req, err := toReq(change.New[0])
			if err != nil {
				return nil, err
			}
			corr = &models.Correction{
				Msg: change.Msgs[0],
				F: func() error {
					return c.createRecord(dc.Name, req)
				},
			}
		case diff2.CHANGE:
			id := change.Old[0].Original.(*domainRecord).ID
			req, err := toReq(change.New[0])
			if err != nil {
				return nil, err
			}
			corr = &models.Correction{
				Msg: fmt.Sprintf("%s, porkbun ID: %s", change.Msgs[0], id),
				F: func() error {
					return c.modifyRecord(dc.Name, id, req)
				},
			}
		case diff2.DELETE:
			id := change.Old[0].Original.(*domainRecord).ID
			corr = &models.Correction{
				Msg: fmt.Sprintf("%s, porkbun ID: %s", change.Msgs[0], id),
				F: func() error {
					return c.deleteRecord(dc.Name, id)
				},
			}
		}
		corrections = append(corrections, corr)
	}

	return corrections, nil
}

// GetZoneRecords gets the records of a zone and returns them in RecordConfig format.
func (c *porkbunProvider) GetZoneRecords(domain string) (models.Records, error) {
	records, err := c.getRecords(domain)
	if err != nil {
		return nil, err
	}
	existingRecords := make([]*models.RecordConfig, len(records))
	for i := range records {
		existingRecords[i] = toRc(domain, &records[i])
	}
	return existingRecords, nil
}

// parses the porkbun format into our standard RecordConfig
func toRc(domain string, r *domainRecord) *models.RecordConfig {
	ttl, _ := strconv.ParseUint(r.TTL, 10, 32)
	priority, _ := strconv.ParseUint(r.Prio, 10, 16)

	rc := &models.RecordConfig{
		Type:         r.Type,
		TTL:          uint32(ttl),
		MxPreference: uint16(priority),
		SrvPriority:  uint16(priority),
		Original:     r,
	}
	rc.SetLabelFromFQDN(r.Name, domain)

	switch rtype := r.Type; rtype { // #rtype_variations
	case "TXT":
		rc.SetTargetTXT(r.Content)
	case "MX", "CNAME", "ALIAS", "NS":
		if strings.HasSuffix(r.Content, ".") {
			rc.SetTarget(r.Content)
		} else {
			rc.SetTarget(r.Content + ".")
		}
	case "CAA":
		// 0, issue, "letsencrypt.org"
		c := strings.Split(r.Content, " ")

		caaFlag, _ := strconv.ParseUint(c[0], 10, 8)
		rc.CaaFlag = uint8(caaFlag)
		rc.CaaTag = c[1]
		rc.SetTarget(strings.ReplaceAll(c[2], "\"", ""))
	case "TLSA":
		// 0 0 0 00000000000000000000000
		c := strings.Split(r.Content, " ")

		tlsaUsage, _ := strconv.ParseUint(c[0], 10, 8)
		rc.TlsaUsage = uint8(tlsaUsage)
		tlsaSelector, _ := strconv.ParseUint(c[1], 10, 8)
		rc.TlsaSelector = uint8(tlsaSelector)
		tlsaMatchingType, _ := strconv.ParseUint(c[2], 10, 8)
		rc.TlsaMatchingType = uint8(tlsaMatchingType)
		rc.SetTarget(c[3])
	case "SRV":
		// 5 5060 sip.example.com
		c := strings.Split(r.Content, " ")

		srvWeight, _ := strconv.ParseUint(c[0], 10, 16)
		rc.SrvWeight = uint16(srvWeight)
		srvPort, _ := strconv.ParseUint(c[1], 10, 16)
		rc.SrvPort = uint16(srvPort)
		rc.SetTarget(c[2])
	default:
		rc.SetTarget(r.Content)
	}

	return rc
}

// toReq takes a RecordConfig and turns it into the native format used by the API.
func toReq(rc *models.RecordConfig) (requestParams, error) {
	req := requestParams{
		"type":    rc.Type,
		"name":    rc.GetLabel(),
		"content": rc.GetTargetField(),
		"ttl":     strconv.Itoa(int(rc.TTL)),
	}

	// porkbun doesn't use "@", it uses an empty name
	if req["name"] == "@" {
		req["name"] = ""
	}

	switch rc.Type { // #rtype_variations
	case "A", "AAAA", "NS", "ALIAS", "CNAME":
	// Nothing special.
	case "TXT":
		req["content"] = rc.GetTargetTXTJoined()
	case "MX":
		req["prio"] = strconv.Itoa(int(rc.MxPreference))
	case "SRV":
		req["prio"] = strconv.Itoa(int(rc.SrvPriority))
		req["content"] = fmt.Sprintf("%d %d %s", rc.SrvWeight, rc.SrvPort, rc.GetTargetField())
	case "CAA":
		req["content"] = fmt.Sprintf("%d %s \"%s\"", rc.CaaFlag, rc.CaaTag, rc.GetTargetField())
	case "TLSA":
		req["content"] = fmt.Sprintf("%d %d %d %s",
			rc.TlsaUsage, rc.TlsaSelector, rc.TlsaMatchingType, rc.GetTargetField())
	default:
		return nil, fmt.Errorf("porkbun.toReq rtype %q unimplemented", rc.Type)
	}

	return req, nil
}

func checkNSModifications(dc *models.DomainConfig) {
	newList := make([]*models.RecordConfig, 0, len(dc.Records))
	for _, rec := range dc.Records {
		if rec.Type == "NS" && rec.GetLabelFQDN() == dc.Name {
			if strings.HasSuffix(rec.GetTargetField(), ".porkbun.com") {
				printer.Warnf("porkbun does not support modifying NS records on base domain. %s will not be added.\n", rec.GetTargetField())
			}
			continue
		}
		newList = append(newList, rec)
	}
	dc.Records = newList
}

func fixTTL(ttl uint32) uint32 {
	if ttl > minimumTTL {
		return ttl
	}
	return minimumTTL
}
package credentials

import (
	"bytes"
	"encoding/binary"
	"errors"
	"github.com/jcmturner/asn1"
	"github.com/jcmturner/gokrb5/types"
	"io/ioutil"
	"strings"
	"time"
	"unsafe"
)

const (
	headerFieldTagKDCOffset = 1
)

// The first byte of the file always has the value 5.
// The value of the second byte contains the version number (1 through 4)
// Versions 1 and 2 of the file format use native byte order for integer representations.
// Versions 3 and 4 always use big-endian byte order
// After the two-byte version indicator, the file has three parts:
//   1) the header (in version 4 only)
//   2) the default principal name
//   3) a sequence of credentials

// CCache is the file credentials cache as define here: https://web.mit.edu/kerberos/krb5-latest/doc/formats/ccache_file_format.html
type CCache struct {
	Version          uint8
	Header           header
	DefaultPrincipal principal
	Credentials      []credential
	Path             string
}

type header struct {
	length uint16
	fields []headerField
}

type headerField struct {
	tag    uint16
	length uint16
	value  []byte
}

// Credential cache entry principal struct.
type principal struct {
	Realm         string
	PrincipalName types.PrincipalName
}

type credential struct {
	Client       principal
	Server       principal
	Key          types.EncryptionKey
	AuthTime     time.Time
	StartTime    time.Time
	EndTime      time.Time
	RenewTill    time.Time
	IsSKey       bool
	TicketFlags  asn1.BitString
	Addresses    []types.HostAddress
	AuthData     []types.AuthorizationDataEntry
	Ticket       []byte
	SecondTicket []byte
}

// LoadCCache loads a credential cache file into a CCache type.
func LoadCCache(cpath string) (CCache, error) {
	k, err := ioutil.ReadFile(cpath)
	if err != nil {
		return CCache{}, err
	}
	c, err := ParseCCache(k)
	c.Path = cpath
	return c, err
}

// ParseCCache byte slice of credential cache data into CCache type.
func ParseCCache(b []byte) (c CCache, err error) {
	p := 0
	//The first byte of the file always has the value 5
	if int8(b[p]) != 5 {
		err = errors.New("Invalid credential cache data. First byte does not equal 5")
		return
	}
	p++
	//Get credential cache version
	//The second byte contains the version number (1 to 4)
	c.Version = uint8(b[p])
	if c.Version < 1 || c.Version > 4 {
		err = errors.New("Invalid credential cache data. Keytab version is not within 1 to 4")
		if err != nil {
			return
		}
	}
	p++
	//Version 1 or 2 of the file format uses native byte order for integer representations. Versions 3 & 4 always uses big-endian byte order
	var endian binary.ByteOrder
	endian = binary.BigEndian
	if (c.Version == 1 || c.Version == 2) && isNativeEndianLittle() {
		endian = binary.LittleEndian
	}
	if c.Version == 4 {
		err = parse_header(b, &p, &c, &endian)
		if err != nil {
			return
		}
	}
	c.DefaultPrincipal = parse_principal(b, &p, &c, &endian)
	for p < len(b) {
		cred, e := parse_credential(b, &p, &c, &endian)
		if e != nil {
			err = e
			return
		}
		c.Credentials = append(c.Credentials, cred)
	}
	return
}

func parse_header(b []byte, p *int, c *CCache, e *binary.ByteOrder) error {
	if c.Version != 4 {
		return errors.New("Credentials cache version is not 4 so there is no header to parse.")
	}
	h := header{}
	h.length = uint16(read_int16(b, p, e))
	for *p <= int(h.length) {
		f := headerField{}
		f.tag = uint16(read_int16(b, p, e))
		f.length = uint16(read_int16(b, p, e))
		f.value = b[*p : *p+int(f.length)]
		*p += int(f.length)
		if !f.valid() {
			return errors.New("Invalid credential cache header found")
		}
		h.fields = append(h.fields, f)
	}
	c.Header = h
	return nil
}

// Parse the Keytab bytes of a principal into a Keytab entry's principal.
func parse_principal(b []byte, p *int, c *CCache, e *binary.ByteOrder) (princ principal) {
	if c.Version != 1 {
		//Name Type is omitted in version 1
		princ.PrincipalName.NameType = int(read_int32(b, p, e))
	}
	nc := int(read_int32(b, p, e))
	if c.Version == 1 {
		//In version 1 the number of components includes the realm. Minus 1 to make consistent with version 2
		nc--
	}
	len_realm := read_int32(b, p, e)
	princ.Realm = string(read_Bytes(b, p, int(len_realm), e))
	for i := 0; i < int(nc); i++ {
		l := read_int32(b, p, e)
		princ.PrincipalName.NameString = append(princ.PrincipalName.NameString, string(read_Bytes(b, p, int(l), e)))
	}
	return princ
}

func parse_credential(b []byte, p *int, c *CCache, e *binary.ByteOrder) (cred credential, err error) {
	cred.Client = parse_principal(b, p, c, e)
	cred.Server = parse_principal(b, p, c, e)
	key := types.EncryptionKey{}
	key.KeyType = int(read_int16(b, p, e))
	if c.Version == 3 {
		//repeated twice in version 3
		key.KeyType = int(read_int16(b, p, e))
	}
	key.KeyValue = read_data(b, p, e)
	cred.Key = key
	cred.AuthTime = read_timestamp(b, p, e)
	cred.StartTime = read_timestamp(b, p, e)
	cred.EndTime = read_timestamp(b, p, e)
	cred.RenewTill = read_timestamp(b, p, e)
	if ik := read_int8(b, p, e); ik == 0 {
		cred.IsSKey = false
	} else {
		cred.IsSKey = true
	}
	cred.TicketFlags = types.NewKrbFlags()
	cred.TicketFlags.Bytes = read_Bytes(b, p, 4, e)
	l := int(read_int32(b, p, e))
	cred.Addresses = make([]types.HostAddress, l, l)
	for i := range cred.Addresses {
		cred.Addresses[i] = read_address(b, p, e)
	}
	l = int(read_int32(b, p, e))
	cred.AuthData = make([]types.AuthorizationDataEntry, l, l)
	for i := range cred.AuthData {
		cred.AuthData[i] = read_authDataEntry(b, p, e)
	}
	cred.Ticket = read_data(b, p, e)
	cred.SecondTicket = read_data(b, p, e)
	return
}

// GetClientPrincipalName returns a PrincipalName type for the client the credentials cache is for.
func (c *CCache) GetClientPrincipalName() types.PrincipalName {
	return c.DefaultPrincipal.PrincipalName
}

// GetClientRealm returns the reals of the client the credentials cache is for.
func (c *CCache) GetClientRealm() string {
	return c.DefaultPrincipal.Realm
}

// GetClientCredentials returns a Credentials object representing the client of the credentials cache.
func (c *CCache) GetClientCredentials() *Credentials {
	return &Credentials{
		Username: c.DefaultPrincipal.PrincipalName.GetPrincipalNameString(),
		Realm:    c.GetClientRealm(),
		CName:    c.DefaultPrincipal.PrincipalName,
	}
}

// Contains tests if the cache contains a credential for the provided server PrincipalName
func (c *CCache) Contains(p types.PrincipalName) bool {
	for _, cred := range c.Credentials {
		if cred.Server.PrincipalName.Equal(p) {
			return true
		}
	}
	return false
}

// GetEntry returns a specific credential for the PrincipalName provided.
func (c *CCache) GetEntry(p types.PrincipalName) (credential, bool) {
	var cred credential
	var found bool
	for i := range c.Credentials {
		if c.Credentials[i].Server.PrincipalName.Equal(p) {
			cred = c.Credentials[i]
			found = true
			break
		}
	}
	if !found {
		return cred, false
	}
	return cred, true
}

// GetEntries filters out configuration entries an returns a slice of credentials.
func (c *CCache) GetEntries() []credential {
	var creds []credential
	for _, cred := range c.Credentials {
		// Filter out configuration entries
		if strings.HasPrefix(cred.Server.Realm, "X-CACHECONF") {
			continue
		}
		creds = append(creds, cred)
	}
	return creds
}

func (h *headerField) valid() bool {
	// At this time there is only one defined header field.
	// Its tag value is 1, its length is always 8.
	// Its contents are two 32-bit integers giving the seconds and microseconds
	// of the time offset of the KDC relative to the client.
	// Adding this offset to the current time on the client should give the current time on the KDC, if that offset has not changed since the initial authentication.

	// Done as a switch in case other tag values are added in the future.
	switch h.tag {
	case headerFieldTagKDCOffset:
		if h.length != 8 || len(h.value) != 8 {
			return false
		}
		return true
	}
	return false
}

func read_data(b []byte, p *int, e *binary.ByteOrder) []byte {
	l := read_int32(b, p, e)
	return read_Bytes(b, p, int(l), e)
}

func read_address(b []byte, p *int, e *binary.ByteOrder) types.HostAddress {
	a := types.HostAddress{}
	a.AddrType = int(read_int16(b, p, e))
	a.Address = read_data(b, p, e)
	return a
}

func read_authDataEntry(b []byte, p *int, e *binary.ByteOrder) types.AuthorizationDataEntry {
	a := types.AuthorizationDataEntry{}
	a.ADType = int(read_int16(b, p, e))
	a.ADData = read_data(b, p, e)
	return a
}

// Read bytes representing a timestamp.
func read_timestamp(b []byte, p *int, e *binary.ByteOrder) time.Time {
	return time.Unix(int64(read_int32(b, p, e)), 0)
}

// Read bytes representing an eight bit integer.
func read_int8(b []byte, p *int, e *binary.ByteOrder) (i int8) {
	buf := bytes.NewBuffer(b[*p : *p+1])
	binary.Read(buf, *e, &i)
	*p++
	return
}

// Read bytes representing a sixteen bit integer.
func read_int16(b []byte, p *int, e *binary.ByteOrder) (i int16) {
	buf := bytes.NewBuffer(b[*p : *p+2])
	binary.Read(buf, *e, &i)
	*p += 2
	return
}

// Read bytes representing a thirty two bit integer.
func read_int32(b []byte, p *int, e *binary.ByteOrder) (i int32) {
	buf := bytes.NewBuffer(b[*p : *p+4])
	binary.Read(buf, *e, &i)
	*p += 4
	return
}

func read_Bytes(b []byte, p *int, s int, e *binary.ByteOrder) []byte {
	buf := bytes.NewBuffer(b[*p : *p+s])
	r := make([]byte, s)
	binary.Read(buf, *e, &r)
	*p += s
	return r
}

func isNativeEndianLittle() bool {
	var x = 0x012345678
	var p = unsafe.Pointer(&x)
	var bp = (*[4]byte)(p)

	var endian bool
	if 0x01 == bp[0] {
		endian = false
	} else if (0x78 & 0xff) == (bp[0] & 0xff) {
		endian = true
	} else {
		// Default to big endian
		endian = false
	}
	return endian
}

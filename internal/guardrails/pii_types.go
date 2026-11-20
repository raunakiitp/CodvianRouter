// G18 Area 7 — extended PII types: validators for the digit/ID
// patterns added to the redact table in guardrails.go.
//
// Every pattern here is implemented from the public format spec of
// the identifier itself (checksum algorithms are standard math —
// Luhn, CPF/CNPJ mod-11, IBAN mod-97, the Italian odd/even table,
// the German Mod-11,10 procedure, the Dutch 11-test — not copied
// from any other router's code). The regex finds candidates; the
// validator decides. That split is what keeps false positives
// down: a 16-digit run that fails Luhn is an order number, not a
// card; a 9-digit run that fails the 11-test is not a BSN.
//
// A validator receives exactly the regex match (no surrounding
// context) and returns whether it is a well-formed value of that
// type. Validators are pure and allocation-light; they run only
// on regex matches, never on the whole text.
package guardrails

import (
	"net"
	"strings"
)

// patternValidators maps a redact-rule pattern label to the
// validator its matches must pass. A rule with no entry here is
// regex-only (its pattern is specific enough on its own).
var patternValidators = map[string]func(string) bool{
	"card":              validCardLuhn,
	"ssn":               validSSN,
	"cpf":               validCPF,
	"cnpj":              validCNPJ,
	"ipv6":              validIPv6,
	"iban":              validIBAN,
	"eu-vat":            validEUVAT,
	"es-dni-nie":        validESID,
	"it-codice-fiscale": validCodiceFiscale,
	"pl-pesel":          validPESEL,
	"nl-bsn":            validBSN,
	"de-steuer-id":      validSteuerID,
	"se-personnummer":   validPersonnummer,
	"za-id":             validZAID,
	"za-vat":            validZAVAT,
	"za-tax-ref":        validZATaxRef,
}

// digitsOnly strips every non-digit from s.
func digitsOnly(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// allSameDigits reports whether s is one digit repeated.
func allSameDigits(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return len(s) > 0
}

// luhnValid runs the Luhn checksum over a digit string: from the
// rightmost digit, double every second digit, subtract 9 from any
// doubled value over 9, and require the total to be a multiple
// of 10.
func luhnValid(digits string) bool {
	if len(digits) < 2 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// validCardLuhn strips separators and requires 13–19 digits that
// pass Luhn. This is what kills the old false positive: any
// 16-digit run (an order number, a timestamp) no longer matches
// unless it is shaped like a real card number.
func validCardLuhn(s string) bool {
	d := digitsOnly(s)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	return luhnValid(d)
}

// validSSN checks the US Social Security Number rules: area
// 001–665 or 667–899 (000, 666 and 900–999 are never issued),
// group 01–99, serial 0001–9999.
func validSSN(s string) bool {
	d := digitsOnly(s)
	if len(d) != 9 {
		return false
	}
	area := int(d[0]-'0')*100 + int(d[1]-'0')*10 + int(d[2]-'0')
	group := int(d[3]-'0')*10 + int(d[4]-'0')
	serial := int(d[5]-'0')*1000 + int(d[6]-'0')*100 + int(d[7]-'0')*10 + int(d[8]-'0')
	if area == 0 || area == 666 || area >= 900 {
		return false
	}
	if group == 0 || serial == 0 {
		return false
	}
	return true
}

// mod11CheckDigit is the shared CPF/CNPJ check-digit rule: sum
// the digits weighted from high to low, take the remainder mod
// 11; a remainder under 2 gives check digit 0, otherwise 11
// minus the remainder.
func mod11CheckDigit(sum int) int {
	if r := sum % 11; r < 2 {
		return 0
	} else {
		return 11 - r
	}
}

// validCPF checks a Brazilian CPF (11 digits): the first check
// digit weights the first 9 digits 10..2, the second weights the
// first 10 digits 11..2. All-same-digit strings are rejected:
// they pass the arithmetic but are never issued.
func validCPF(s string) bool {
	d := digitsOnly(s)
	if len(d) != 11 || allSameDigits(d) {
		return false
	}
	sum := 0
	for i := 0; i < 9; i++ {
		sum += int(d[i]-'0') * (10 - i)
	}
	if mod11CheckDigit(sum) != int(d[9]-'0') {
		return false
	}
	sum = 0
	for i := 0; i < 10; i++ {
		sum += int(d[i]-'0') * (11 - i)
	}
	return mod11CheckDigit(sum) == int(d[10]-'0')
}

// validCNPJ checks a Brazilian CNPJ (14 digits): the first check
// digit weights the first 12 digits 5,4,3,2,9,8,7,6,5,4,3,2 and
// the second weights the first 13 digits 6,5,4,3,2,9,8,7,6,5,4,3,2.
func validCNPJ(s string) bool {
	d := digitsOnly(s)
	if len(d) != 14 || allSameDigits(d) {
		return false
	}
	w1 := []int{5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
	sum := 0
	for i := 0; i < 12; i++ {
		sum += int(d[i]-'0') * w1[i]
	}
	if mod11CheckDigit(sum) != int(d[12]-'0') {
		return false
	}
	w2 := []int{6, 5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
	sum = 0
	for i := 0; i < 13; i++ {
		sum += int(d[i]-'0') * w2[i]
	}
	return mod11CheckDigit(sum) == int(d[13]-'0')
}

// validIPv6 accepts a candidate only if it parses as an IP with
// at least two colons (a compressed "::" or a full form) and is
// not an IPv4 address. The regex is deliberately loose
// ([0-9A-Fa-f:]+) so it can find candidates anywhere; net.ParseIP
// is the real test — a time ("12:34:56") or a hex word fails it.
func validIPv6(s string) bool {
	if !strings.Contains(s, ":") {
		return false
	}
	if strings.Count(s, ":") < 2 && !strings.Contains(s, "::") {
		return false
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	return ip.To4() == nil
}

// ibanCountryLengths is the IBAN total length (letters + digits)
// per country code, for the countries this router recognises. A
// code outside the table is rejected rather than guessed at.
var ibanCountryLengths = map[string]int{
	"AD": 24, "AL": 28, "AT": 20, "BE": 16, "BG": 22, "CH": 21,
	"CY": 28, "CZ": 24, "DE": 22, "DK": 18, "EE": 20, "ES": 24,
	"FI": 18, "FR": 27, "GB": 22, "GR": 27, "HR": 21, "HU": 28,
	"IE": 22, "IS": 26, "IT": 27, "LI": 21, "LT": 20, "LU": 20,
	"LV": 21, "MC": 27, "MT": 31, "NL": 18, "NO": 15, "PL": 28,
	"PT": 25, "RO": 24, "SE": 24, "SI": 19, "SK": 24, "SM": 27,
	"VA": 22,
}

// validIBAN strips spaces, checks the country code and total
// length, then runs the mod-97 test: move the first four
// characters to the end, expand letters A=10..Z=35, and require
// the resulting number mod 97 to equal 1.
func validIBAN(s string) bool {
	iban := strings.ReplaceAll(s, " ", "")
	if len(iban) < 15 || len(iban) > 34 {
		return false
	}
	want, ok := ibanCountryLengths[iban[:2]]
	if !ok || len(iban) != want {
		return false
	}
	for i := 0; i < len(iban); i++ {
		c := iban[i]
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	rearranged := iban[4:] + iban[:4]
	rem := 0
	for i := 0; i < len(rearranged); i++ {
		c := rearranged[i]
		if c >= '0' && c <= '9' {
			rem = (rem*10 + int(c-'0')) % 97
		} else {
			v := int(c-'A') + 10
			rem = (rem*10 + v/10) % 97
			rem = (rem*10 + v%10) % 97
		}
	}
	return rem == 1
}

// euVATFormat is one country's VAT-number shape after the
// two-letter prefix: the accepted total lengths and whether the
// rest must be digits only. This is format/length validation,
// not a national checksum: VAT check algorithms differ per
// country and several countries publish none, so claiming a
// checksum pass would be false precision.
type euVATFormat struct {
	lengths    []int
	digitsOnly bool
}

var euVATFormats = map[string]euVATFormat{
	"AT": {[]int{9}, false}, // ATU + 8 digits
	"BE": {[]int{10}, true},
	"BG": {[]int{9, 10}, true},
	"CY": {[]int{8}, false},
	"CZ": {[]int{8, 9, 10}, true},
	"DE": {[]int{9}, true},
	"DK": {[]int{8}, true},
	"EE": {[]int{9}, true},
	"ES": {[]int{9}, false},
	"FI": {[]int{8}, true},
	"FR": {[]int{11}, false}, // 2 check chars + 9-digit SIREN
	"GR": {[]int{9}, true},
	"HR": {[]int{11}, true},
	"HU": {[]int{8}, true},
	"IE": {[]int{8, 9}, false},
	"IT": {[]int{11}, true},
	"LT": {[]int{9, 12}, false},
	"LU": {[]int{8}, true},
	"LV": {[]int{11}, true},
	"MT": {[]int{8}, true},
	"NL": {[]int{12}, false}, // 9 digits + B + 2 digits
	"PL": {[]int{10}, true},
	"PT": {[]int{9}, true},
	"RO": {[]int{2, 3, 4, 5, 6, 7, 8, 9, 10}, true},
	"SE": {[]int{12}, true},
	"SI": {[]int{8}, true},
	"SK": {[]int{10}, true},
}

// validEUVAT checks the country prefix against the length table
// above. "DE136695976" passes; "DE123" fails on length.
func validEUVAT(s string) bool {
	if len(s) < 4 {
		return false
	}
	f, ok := euVATFormats[s[:2]]
	if !ok {
		return false
	}
	rest := s[2:]
	okLen := false
	for _, n := range f.lengths {
		if len(rest) == n {
			okLen = true
			break
		}
	}
	if !okLen {
		return false
	}
	if f.digitsOnly && digitsOnly(rest) != rest {
		return false
	}
	return true
}

// dniLetter is the Spanish DNI/NIE check letter: the number mod
// 23 indexes "TRWAGMYFPDXBNJZSQVHLCKE".
func dniLetter(num int) byte {
	return "TRWAGMYFPDXBNJZSQVHLCKE"[num%23]
}

// validESID checks a Spanish DNI (8 digits + letter) or NIE
// (X/Y/Z + 7 digits + letter; X=0, Y=1, Z=2 for the check).
func validESID(s string) bool {
	if len(s) != 9 {
		return false
	}
	if s[0] >= '0' && s[0] <= '9' {
		num := 0
		for i := 0; i < 8; i++ {
			if s[i] < '0' || s[i] > '9' {
				return false
			}
			num = num*10 + int(s[i]-'0')
		}
		return dniLetter(num) == s[8]
	}
	var prefix int
	switch s[0] {
	case 'X':
		prefix = 0
	case 'Y':
		prefix = 1
	case 'Z':
		prefix = 2
	default:
		return false
	}
	num := prefix
	for i := 1; i < 8; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		num = num*10 + int(s[i]-'0')
	}
	return dniLetter(num) == s[8]
}

// codiceFiscaleOdd maps a character at an odd (1-based) position
// of a codice fiscale to its value; even positions use the
// character's plain value (digit, or letter A=0..Z=25).
func codiceFiscaleOdd(c byte) int {
	if c >= '0' && c <= '9' {
		return []int{1, 0, 5, 7, 9, 13, 15, 17, 19, 21}[c-'0']
	}
	return []int{1, 0, 5, 7, 9, 13, 15, 17, 19, 21, 2, 4, 18, 20, 11, 3, 6, 8, 12, 14, 16, 10, 22, 25, 24, 23}[c-'A']
}

// validCodiceFiscale checks an Italian codice fiscale: 16
// characters, where the last is the check letter — the sum of
// the odd-position values and even-position values over the
// first 15 characters, mod 26, indexes A..Z.
func validCodiceFiscale(s string) bool {
	if len(s) != 16 {
		return false
	}
	sum := 0
	for i := 0; i < 15; i++ {
		c := s[i]
		if i%2 == 0 { // 1-based odd position
			sum += codiceFiscaleOdd(c)
		} else if c >= '0' && c <= '9' {
			sum += int(c - '0')
		} else {
			sum += int(c - 'A')
		}
	}
	return byte('A'+sum%26) == s[15]
}

// validPESEL checks a Polish PESEL (11 digits): weights
// 1,3,7,9,1,3,7,9,1,3 over the first 10 digits; the check digit
// is (10 - sum%10) % 10.
func validPESEL(s string) bool {
	d := digitsOnly(s)
	if len(d) != 11 {
		return false
	}
	w := []int{1, 3, 7, 9, 1, 3, 7, 9, 1, 3}
	sum := 0
	for i := 0; i < 10; i++ {
		sum += int(d[i]-'0') * w[i]
	}
	return (10-sum%10)%10 == int(d[10]-'0')
}

// validBSN runs the Dutch 11-test over a 9-digit BSN:
// 9*d1 + 8*d2 + ... + 2*d8 - d9 must be divisible by 11.
func validBSN(s string) bool {
	d := digitsOnly(s)
	if len(d) != 9 {
		return false
	}
	sum := 0
	for i := 0; i < 8; i++ {
		sum += (9 - i) * int(d[i]-'0')
	}
	sum -= int(d[8] - '0')
	return sum%11 == 0
}

// validSteuerID checks a German Steueridentifikationsnummer (11
// digits) with the ISO 7064 Mod 11,10 procedure: product starts
// at 10; for each of the first 10 digits, sum = (digit +
// product) mod 10 (0 becomes 10), product = (2*sum) mod 11;
// the check digit is (11 - product) mod 10 (10 becomes 0).
// Only the checksum and a non-zero first digit are enforced:
// the additional plausibility rule about repeated digits is
// deliberately NOT enforced, to avoid false negatives on
// validly issued numbers.
func validSteuerID(s string) bool {
	d := digitsOnly(s)
	if len(d) != 11 || d[0] == '0' {
		return false
	}
	product := 10
	for i := 0; i < 10; i++ {
		sum := (int(d[i]-'0') + product) % 10
		if sum == 0 {
			sum = 10
		}
		product = (2 * sum) % 11
	}
	check := (11 - product) % 10
	return check == int(d[10]-'0')
}

// validPersonnummer checks a Swedish personnummer: 10 digits
// (YYMMDD + 4) or 12 digits (YYYYMMDD + 4), optional -/+
// separator, month 01–12, day 01–31, and Luhn over the last 10
// digits.
func validPersonnummer(s string) bool {
	d := digitsOnly(s)
	if len(d) != 10 && len(d) != 12 {
		return false
	}
	date := d
	if len(d) == 12 {
		date = d[2:]
	}
	month := int(date[2]-'0')*10 + int(date[3]-'0')
	day := int(date[4]-'0')*10 + int(date[5]-'0')
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return false
	}
	return luhnValid(d[len(d)-10:])
}

// plausibleDate checks a YYMMDD prefix for month 01–12 and day
// 01–31 (no leap-year check: 29 February is accepted, so a
// valid leap-day birth is never rejected).
func plausibleDate(yyMMDD string) bool {
	if len(yyMMDD) != 6 {
		return false
	}
	month := int(yyMMDD[2]-'0')*10 + int(yyMMDD[3]-'0')
	day := int(yyMMDD[4]-'0')*10 + int(yyMMDD[5]-'0')
	return month >= 1 && month <= 12 && day >= 1 && day <= 31
}

// validZAID checks a South African ID number (13 digits): a
// plausible YYMMDD birth date in the first 6 digits and Luhn
// over all 13 digits.
func validZAID(s string) bool {
	d := digitsOnly(s)
	if len(d) != 13 || !plausibleDate(d[:6]) {
		return false
	}
	return luhnValid(d)
}

// validZAVAT checks a South African VAT number: 10 digits
// starting with 4. Format-only: SARS publishes no public
// checksum for VAT numbers, so no checksum is claimed.
func validZAVAT(s string) bool {
	d := digitsOnly(s)
	return len(d) == 10 && d[0] == '4'
}

// validZATaxRef checks a South African tax reference number: 10
// digits whose first digit is 1, 2, 3 or 5. This is the weakest
// validator in this file — SARS publishes no public checksum and
// a bare 10-digit run is genuinely ambiguous, so the validator
// only excludes the digit ranges already owned by other rules
// (6–9: phone, 0: ZA phone, 4: ZA VAT). The rule runs last among
// the 10-digit shapes for exactly that reason; operators who
// see false positives should disable the za-tax-ref type.
func validZATaxRef(s string) bool {
	d := digitsOnly(s)
	if len(d) != 10 {
		return false
	}
	switch d[0] {
	case '1', '2', '3', '5':
		return true
	}
	return false
}

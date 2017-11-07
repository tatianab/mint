package mint

import (
	"encoding/hex"
	"fmt"
	"time"
)

func VersionNegotiation(offered, supported []uint16) (bool, uint16) {
	for _, offeredVersion := range offered {
		for _, supportedVersion := range supported {
			logf(logTypeHandshake, "[server] version offered by client [%04x] <> [%04x]", offeredVersion, supportedVersion)
			if offeredVersion == supportedVersion {
				// XXX: Should probably be highest supported version, but for now, we
				// only support one version, so it doesn't really matter.
				return true, offeredVersion
			}
		}
	}

	return false, 0
}

func DHNegotiation(keyShares []KeyShareEntry, groups []NamedGroup) (bool, NamedGroup, []byte, []byte) {
	for _, share := range keyShares {
		for _, group := range groups {
			if group != share.Group {
				continue
			}

			pub, priv, err := newKeyShare(share.Group)
			if err != nil {
				// If we encounter an error, just keep looking
				continue
			}

			dhSecret, err := keyAgreement(share.Group, share.KeyExchange, priv)
			if err != nil {
				// If we encounter an error, just keep looking
				continue
			}

			return true, group, pub, dhSecret
		}
	}

	return false, 0, nil, nil
}

const (
	ticketAgeTolerance uint32 = 5 * 1000 // five seconds in milliseconds
)

func PSKNegotiation(identities []PSKIdentity, psks PreSharedKeyCache) (bool, int, *PreSharedKey, CipherSuiteParams, error) {
	logf(logTypeNegotiation, "Negotiating PSK offered=[%d] supported=[%d]", len(identities), psks.Size())
	for i, id := range identities {
		identityHex := hex.EncodeToString(id.Identity)

		psk, ok := psks.Get(identityHex)
		if !ok {
			logf(logTypeNegotiation, "No PSK for identity %x", identityHex)
			continue
		}

		// For resumption, make sure the ticket age is correct
		if psk.IsResumption {
			extTicketAge := id.ObfuscatedTicketAge - psk.TicketAgeAdd
			knownTicketAge := uint32(time.Since(psk.ReceivedAt) / time.Millisecond)
			ticketAgeDelta := knownTicketAge - extTicketAge
			if knownTicketAge < extTicketAge {
				ticketAgeDelta = extTicketAge - knownTicketAge
			}
			if ticketAgeDelta > ticketAgeTolerance {
				logf(logTypeNegotiation, "WARNING potential replay [%x]", psk.Identity)
				logf(logTypeNegotiation, "Ticket age exceeds tolerance |%d - %d| = [%d] > [%d]",
					extTicketAge, knownTicketAge, ticketAgeDelta, ticketAgeTolerance)
				return false, 0, nil, CipherSuiteParams{}, fmt.Errorf("WARNING Potential replay for identity %x", psk.Identity)
			}
		}

		params, ok := cipherSuiteMap[psk.CipherSuite]
		if !ok {
			return false, 0, nil, CipherSuiteParams{}, fmt.Errorf("tls.cryptoinit: Unsupported ciphersuite from PSK [%04x]", psk.CipherSuite)
		}

		logf(logTypeNegotiation, "Using PSK with identity %x", psk.Identity)
		return true, i, &psk, params, nil
	}

	logf(logTypeNegotiation, "Failed to find a usable PSK")
	return false, 0, nil, CipherSuiteParams{}, nil
}

func PSKModeNegotiation(canDoDH, canDoPSK bool, modes []PSKKeyExchangeMode) (bool, bool) {
	logf(logTypeNegotiation, "Negotiating PSK modes [%v] [%v] [%+v]", canDoDH, canDoPSK, modes)
	dhAllowed := false
	dhRequired := true
	for _, mode := range modes {
		dhAllowed = dhAllowed || (mode == PSKModeDHEKE)
		dhRequired = dhRequired && (mode == PSKModeDHEKE)
	}

	// Use PSK if we can meet DH requirement and modes were provided
	usingPSK := canDoPSK && (!dhRequired || canDoDH) && (len(modes) > 0)

	// Use DH if allowed
	usingDH := canDoDH && (dhAllowed || !usingPSK)

	logf(logTypeNegotiation, "Results of PSK mode negotiation: usingDH=[%v] usingPSK=[%v]", usingDH, usingPSK)
	return usingDH, usingPSK
}

func CertificateSelection(serverName *string, signatureSchemes []SignatureScheme, certs []*Certificate) (*Certificate, SignatureScheme, error) {
	// Select for server name if provided
	candidates := certs
	if serverName != nil {
		candidatesByName := []*Certificate{}
		for _, cert := range certs {
			for _, name := range cert.Chain[0].DNSNames {
				if len(*serverName) > 0 && name == *serverName {
					candidatesByName = append(candidatesByName, cert)
				}
			}
		}

		if len(candidatesByName) == 0 {
			return nil, 0, fmt.Errorf("No certificates available for server name")
		}

		candidates = candidatesByName
	}

	// Select for signature scheme
	for _, cert := range candidates {
		for _, scheme := range signatureSchemes {
			if !schemeValidForKey(scheme, cert.PrivateKey) {
				continue
			}

			return cert, scheme, nil
		}
	}

	return nil, 0, fmt.Errorf("No certificates compatible with signature schemes")
}

func EarlyDataNegotiation(usingPSK, gotEarlyData, allowEarlyData bool) bool {
	usingEarlyData := gotEarlyData && usingPSK && allowEarlyData
	logf(logTypeNegotiation, "Early data negotiation (%v, %v, %v) => %v", usingPSK, gotEarlyData, allowEarlyData, usingEarlyData)
	return usingEarlyData
}

func CipherSuiteNegotiation(psk *PreSharedKey, offered, supported []CipherSuite) (CipherSuite, error) {
	for _, s1 := range offered {
		if psk != nil {
			if s1 == psk.CipherSuite {
				return s1, nil
			}
			continue
		}

		for _, s2 := range supported {
			if s1 == s2 {
				return s1, nil
			}
		}
	}

	return 0, fmt.Errorf("No overlap between offered and supproted ciphersuites (psk? [%v])", psk != nil)
}

func ALPNNegotiation(psk *PreSharedKey, offered, supported []string) (string, error) {
	for _, p1 := range offered {
		if psk != nil {
			if p1 != psk.NextProto {
				continue
			}
		}

		for _, p2 := range supported {
			if p1 == p2 {
				return p1, nil
			}
		}
	}

	// If the client offers ALPN on resumption, it must match the earlier one
	var err error
	if psk != nil && psk.IsResumption && (len(offered) > 0) {
		err = fmt.Errorf("ALPN for PSK not provided")
	}
	return "", err
}

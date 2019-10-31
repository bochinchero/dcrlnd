package hop

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/decred/dcrlnd/lnwire"
	"github.com/decred/dcrlnd/record"
	"github.com/decred/dcrlnd/tlv"
	"github.com/decred/lightning-onion/v2"
)

// PayloadViolation is an enum encapsulating the possible invalid payload
// violations that can occur when processing or validating a payload.
type PayloadViolation byte

const (
	// OmittedViolation indicates that a type was expected to be found the
	// payload but was absent.
	OmittedViolation PayloadViolation = iota

	// IncludedViolation indicates that a type was expected to be omitted
	// from the payload but was present.
	IncludedViolation

	// RequiredViolation indicates that an unknown even type was found in
	// the payload that we could not process.
	RequiredViolation
)

// String returns a human-readable description of the violation as a verb.
func (v PayloadViolation) String() string {
	switch v {
	case OmittedViolation:
		return "omitted"

	case IncludedViolation:
		return "included"

	case RequiredViolation:
		return "required"

	default:
		return "unknown violation"
	}
}

// ErrInvalidPayload is an error returned when a parsed onion payload either
// included or omitted incorrect records for a particular hop type.
type ErrInvalidPayload struct {
	// Type the record's type that cause the violation.
	Type tlv.Type

	// Violation is an enum indicating the type of violation detected in
	// processing Type.
	Violation PayloadViolation

	// FinalHop if true, indicates that the violation is for the final hop
	// in the route (identified by next hop id), otherwise the violation is
	// for an intermediate hop.
	FinalHop bool
}

// Error returns a human-readable description of the invalid payload error.
func (e ErrInvalidPayload) Error() string {
	hopType := "intermediate"
	if e.FinalHop {
		hopType = "final"
	}

	return fmt.Sprintf("onion payload for %s hop %v record with type %d",
		hopType, e.Violation, e.Type)
}

// Payload encapsulates all information delivered to a hop in an onion payload.
// A Hop can represent either a TLV or legacy payload. The primary forwarding
// instruction can be accessed via ForwardingInfo, and additional records can be
// accessed by other member functions.
type Payload struct {
	// FwdInfo holds the basic parameters required for HTLC forwarding, e.g.
	// amount, cltv, and next hop.
	FwdInfo ForwardingInfo
}

// NewLegacyPayload builds a Payload from the amount, cltv, and next hop
// parameters provided by leegacy onion payloads.
func NewLegacyPayload(f *sphinx.HopData) *Payload {
	nextHop := binary.BigEndian.Uint64(f.NextAddress[:])

	return &Payload{
		FwdInfo: ForwardingInfo{
			Network:         DecredNetwork,
			NextHop:         lnwire.NewShortChanIDFromInt(nextHop),
			AmountToForward: lnwire.MilliAtom(f.ForwardAmount),
			OutgoingCTLV:    f.OutgoingCltv,
		},
	}
}

// NewPayloadFromReader builds a new Hop from the passed io.Reader. The reader
// should correspond to the bytes encapsulated in a TLV onion payload.
func NewPayloadFromReader(r io.Reader) (*Payload, error) {
	var (
		cid  uint64
		amt  uint64
		cltv uint32
	)

	tlvStream, err := tlv.NewStream(
		record.NewAmtToFwdRecord(&amt),
		record.NewLockTimeRecord(&cltv),
		record.NewNextHopIDRecord(&cid),
	)
	if err != nil {
		return nil, err
	}

	parsedTypes, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		// Promote any required type failures into ErrInvalidPayload.
		if e, required := err.(tlv.ErrUnknownRequiredType); required {
			// NOTE: FinalHop will be incorrect if the unknown
			// required was type 0. Otherwise, the failure must have
			// occurred after type 6 and cid should contain an
			// accurate value.
			return nil, ErrInvalidPayload{
				Type:      tlv.Type(e),
				Violation: RequiredViolation,
				FinalHop:  cid == 0,
			}
		}
		return nil, err
	}

	nextHop := lnwire.NewShortChanIDFromInt(cid)

	// Validate whether the sender properly included or omitted tlv records
	// in accordance with BOLT 04.
	err = ValidateParsedPayloadTypes(parsedTypes, nextHop)
	if err != nil {
		return nil, err
	}

	return &Payload{
		FwdInfo: ForwardingInfo{
			Network:         DecredNetwork,
			NextHop:         nextHop,
			AmountToForward: lnwire.MilliAtom(amt),
			OutgoingCTLV:    cltv,
		},
	}, nil
}

// ForwardingInfo returns the basic parameters required for HTLC forwarding,
// e.g. amount, cltv, and next hop.
func (h *Payload) ForwardingInfo() ForwardingInfo {
	return h.FwdInfo
}

// ValidateParsedPayloadTypes checks the types parsed from a hop payload to
// ensure that the proper fields are either included or omitted. The finalHop
// boolean should be true if the payload was parsed for an exit hop. The
// requirements for this method are described in BOLT 04.
func ValidateParsedPayloadTypes(parsedTypes tlv.TypeSet,
	nextHop lnwire.ShortChannelID) error {

	isFinalHop := nextHop == Exit

	_, hasAmt := parsedTypes[record.AmtOnionType]
	_, hasLockTime := parsedTypes[record.LockTimeOnionType]
	_, hasNextHop := parsedTypes[record.NextHopOnionType]

	switch {

	// All hops must include an amount to forward.
	case !hasAmt:
		return ErrInvalidPayload{
			Type:      record.AmtOnionType,
			Violation: OmittedViolation,
			FinalHop:  isFinalHop,
		}

	// All hops must include a cltv expiry.
	case !hasLockTime:
		return ErrInvalidPayload{
			Type:      record.LockTimeOnionType,
			Violation: OmittedViolation,
			FinalHop:  isFinalHop,
		}

	// The exit hop should omit the next hop id. If nextHop != Exit, the
	// sender must have included a record, so we don't need to test for its
	// inclusion at intermediate hops directly.
	case isFinalHop && hasNextHop:
		return ErrInvalidPayload{
			Type:      record.NextHopOnionType,
			Violation: IncludedViolation,
			FinalHop:  true,
		}
	}

	return nil
}

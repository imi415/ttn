// Copyright © 2015 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package refactor

import (
	"fmt"
	"io"

	"github.com/TheThingsNetwork/ttn/utils/errors"
	"github.com/TheThingsNetwork/ttn/utils/readwriter"
	"github.com/brocaar/lorawan"
)

type RPacket interface {
	Packet
	Metadata() Metadata
	Payload() lorawan.PHYPayload
	DevEUI() lorawan.EUI64
}

// rpacket implements the core.RPacket interface
type rpacket struct {
	metadata Metadata
	payload  lorawan.PHYPayload
}

// NewRPacket construct a new router packet given a payload and metadata
func NewRPacket(payload lorawan.PHYPayload, metadata Metadata) (RPacket, error) {
	packet := rpacket{payload: payload, metadata: metadata}

	// Check and extract the devEUI
	if payload.MACPayload == nil {
		return nil, errors.New(errors.Structural, "MACPAyload should not be empty")
	}

	_, ok := payload.MACPayload.(*lorawan.MACPayload)
	if !ok {
		return nil, errors.New(errors.Structural, "Packet does not carry a MACPayload")
	}

	return &packet, nil
}

// DevEUI implements the core.BPacket interface
func (p rpacket) DevEUI() lorawan.EUI64 {
	var devEUI lorawan.EUI64
	copy(devEUI[4:], p.payload.MACPayload.(*lorawan.MACPayload).FHDR.DevAddr[:])
	return devEUI
}

// Metadata implements the core.RPacket interface
func (p rpacket) Metadata() Metadata {
	return p.metadata
}

// Payload implements the core.RPacket interface
func (p rpacket) Payload() lorawan.PHYPayload {
	return p.payload
}

// MarshalBinary implements the encoding.BinaryMarshaler interface
func (p rpacket) MarshalBinary() ([]byte, error) {
	var mtype byte
	switch p.payload.MHDR.MType {
	case lorawan.JoinRequest:
		fallthrough
	case lorawan.UnconfirmedDataUp:
		fallthrough
	case lorawan.ConfirmedDataUp:
		mtype = 1 // Up
	case lorawan.JoinAccept:
		fallthrough
	case lorawan.UnconfirmedDataDown:
		fallthrough
	case lorawan.ConfirmedDataDown:
		mtype = 2 // Down
	default:
		msg := fmt.Sprintf("Unsupported mtype: %s", p.payload.MHDR.MType.String())
		return nil, errors.New(errors.Implementation, msg)
	}

	dataMetadata, err := p.metadata.MarshalJSON()
	if err != nil {
		return nil, errors.New(errors.Structural, err)
	}

	dataPayload, err := p.payload.MarshalBinary()
	if err != nil {
		return nil, errors.New(errors.Structural, err)
	}

	rw := readwriter.New(nil)
	rw.Write([]byte{mtype})
	rw.Write(dataMetadata)
	rw.Write(dataPayload)
	return rw.Bytes()
}

// UnmarshalBinary implements the encoding.BinaryUnmarshaler interface
func (p *rpacket) UnmarshalBinary(data []byte) error {
	if p == nil {
		return errors.New(errors.Structural, "Cannot unmarshal nil packet")
	}
	var isUp bool
	rw := readwriter.New(data)
	rw.Read(func(data []byte) {
		if data[0] == 1 {
			isUp = true
		}
	})

	var dataMetadata []byte
	rw.Read(func(data []byte) { dataMetadata = data })

	var dataPayload []byte
	rw.Read(func(data []byte) { dataPayload = data })

	if rw.Err() != nil {
		return errors.New(errors.Structural, rw.Err())
	}

	p.metadata = Metadata{}
	if err := p.metadata.UnmarshalJSON(dataMetadata); err != nil {
		return errors.New(errors.Structural, err)
	}

	p.payload = lorawan.NewPHYPayload(isUp)
	if err := p.payload.UnmarshalBinary(dataPayload); err != nil {
		return errors.New(errors.Structural, err)
	}

	return nil
}

// String implements the Stringer interface
func (p rpacket) String() string {
	str := "Packet {"
	str += fmt.Sprintf("\n\t%s}", p.metadata.String())
	str += fmt.Sprintf("\n\tPayload%+v\n}", p.payload)
	return str
}

type BPacket interface {
	Packet
	Commands() []lorawan.MACCommand
	DevEUI() lorawan.EUI64
	FCnt() uint32
	Metadata() Metadata
	Payload() []byte
	ValidateMIC(key lorawan.AES128Key) (bool, error)
}

// bpacket implements the core.BPacket interface
type bpacket struct {
	rpacket
}

// NewBPacket constructs a new broker packets given a payload and metadata
func NewBPacket(payload lorawan.PHYPayload, metadata Metadata) (BPacket, error) {
	packet, err := NewRPacket(payload, metadata)
	if err != nil {
		return nil, errors.New(errors.Structural, err)
	}

	macPayload := packet.Payload().MACPayload.(*lorawan.MACPayload)
	if len(macPayload.FRMPayload) != 1 {
		return nil, errors.New(errors.Structural, "Invalid frame payload. Expected exactly 1")
	}

	_, ok := macPayload.FRMPayload[0].(*lorawan.DataPayload)
	if !ok {
		return nil, errors.New(errors.Structural, "Invalid frame payload. Expected only data")
	}

	return bpacket{rpacket: packet.(rpacket)}, nil
}

// FCnt implements the core.BPacket interface
func (p bpacket) FCnt() uint32 {
	return p.payload.MACPayload.(*lorawan.MACPayload).FHDR.FCnt
}

// Payload implements the core.BPacket interface
func (p bpacket) Payload() []byte {
	macPayload := p.rpacket.payload.MACPayload.(*lorawan.MACPayload)
	return macPayload.FRMPayload[0].(*lorawan.DataPayload).Bytes
}

// ValidateMIC implements the core.BPacket interface
func (p bpacket) ValidateMIC(key lorawan.AES128Key) (bool, error) {
	return p.rpacket.payload.ValidateMIC(key)
}

// Commands implements the core.BPacket interface
func (p bpacket) Commands() []lorawan.MACCommand {
	return p.rpacket.payload.MACPayload.(*lorawan.MACPayload).FHDR.FOpts
}

type HPacket interface {
	Packet
	AppEUI() lorawan.EUI64
	DevEUI() lorawan.EUI64
	Payload() []byte    // FRMPayload
	Metadata() Metadata // TTL on down, DutyCycle + Rssi on Up
}

// hpacket implements the HPacket interface
type hpacket struct {
	*basehpacket
	metadata Metadata
}

// NewHPacket constructs a new Handler packet
func NewHPacket(appEUI lorawan.EUI64, devEUI lorawan.EUI64, payload []byte, metadata Metadata) HPacket {
	if payload == nil {
		payload = make([]byte, 0)
	}
	return &hpacket{
		basehpacket: &basehpacket{
			appEUI:  appEUI,
			devEUI:  devEUI,
			payload: payload,
		},
		metadata: metadata,
	}
}

// Metadata implements the core.Metadata interface
func (p hpacket) Metadata() Metadata {
	return p.metadata
}

// MarshalBinary implements the encoding.BinaryMarshaler interface
func (p hpacket) MarshalBinary() ([]byte, error) {
	dataMetadata, err := p.Metadata().MarshalJSON()
	if err != nil {
		return nil, errors.New(errors.Structural, err)
	}

	data, err := p.basehpacket.MarshalBinary()
	if err != nil {
		return nil, errors.New(errors.Structural, err)
	}

	rw := readwriter.New(data)
	rw.Write(dataMetadata)
	return rw.Bytes()
}

// UnmarshalBinary implements the encoding.BinaryUnmarshaler interface
func (p *hpacket) UnmarshalBinary(data []byte) error {
	rw := readwriter.New(data)
	rw.Read(func(data []byte) { copy(p.basehpacket.appEUI[:], data) })
	rw.Read(func(data []byte) { copy(p.basehpacket.devEUI[:], data) })
	rw.Read(func(data []byte) { p.basehpacket.payload = data })
	var dataMetadata []byte
	rw.Read(func(data []byte) { dataMetadata = data })
	if err := p.metadata.UnmarshalJSON(dataMetadata); err != nil {
		return errors.New(errors.Structural, err)
	}
	return nil
}

// String implements the fmt.Stringer interface
func (p hpacket) String() string {
	str := "Packet {"
	str += fmt.Sprintf("\n\t%s}", p.metadata.String())
	str += fmt.Sprintf("\n\tAppEUI:%+x\n,", p.appEUI)
	str += fmt.Sprintf("\n\tDevEUI:%+x\n,", p.devEUI)
	str += fmt.Sprintf("\n\tPayload:%v\n}", p.Payload)
	return str
}

type APacket interface {
	Packet
	Payload() []byte
	Metadata() []Metadata
}

// apacket implements the core.APacket interface
type apacket struct {
	payload  []byte
	metadata []Metadata
}

// NewAPacket constructs a new application packet
func NewAPacket(payload []byte, metadata []Metadata) (APacket, error) {
	if len(payload) == 0 {
		return nil, errors.New(errors.Structural, "Application packet must hold a payload")
	}

	return &apacket{payload: payload, metadata: metadata}, nil
}

// Payload implements the core.APacket interface
func (p apacket) Payload() []byte {
	return p.payload
}

// Metadata implements the core.Metadata interface
func (p apacket) Metadata() []Metadata {
	return p.metadata
}

// MarshalBinary implements the encoding.BinaryMarshaler interface
func (p apacket) MarshalBinary() ([]byte, error) {
	rw := readwriter.New(nil)
	for _, m := range p.metadata {
		data, err := m.MarshalJSON()
		if err != nil {
			return nil, errors.New(errors.Structural, err)
		}
		rw.Write(data)
	}
	data, err := rw.Bytes()
	if err != nil {
		return nil, errors.New(errors.Structural, err)
	}

	rw = readwriter.New(nil)
	rw.Write(p.payload)
	rw.Write(data)

	return rw.Bytes()
}

// UnmarshalBinary implements the encoding.BinaryUnmarshaler interface
func (p *apacket) UnmarshalBinary(data []byte) error {
	if p == nil {
		return errors.New(errors.Structural, "Cannot unmarshal nil apacket")
	}

	var dataMetadata []byte
	rw := readwriter.New(data)
	rw.Read(func(data []byte) { p.payload = data })
	rw.Read(func(data []byte) { dataMetadata = data })
	if rw.Err() != nil {
		return errors.New(errors.Structural, rw.Err())
	}

	p.metadata = make([]Metadata, 0)
	rw = readwriter.New(dataMetadata)
	for {
		var dataMetadata []byte
		rw.Read(func(data []byte) { dataMetadata = data })
		if rw.Err() != nil {
			err, ok := rw.Err().(errors.Failure)
			if ok && err.Fault == io.EOF {
				break
			}
			return errors.New(errors.Structural, rw.Err())
		}
		metadata := new(Metadata)
		if err := metadata.UnmarshalJSON(dataMetadata); err != nil {
			return errors.New(errors.Structural, err)
		}

		p.metadata = append(p.metadata, *metadata)
	}

	return nil
}

// String implements the fmt.Stringer interface
func (p apacket) String() string {
	return "TODO"
}

type JoinPacket interface {
	Packet
	AppEUI() lorawan.EUI64
	DevEUI() lorawan.EUI64
	DevNonce() [2]byte
	Metadata() Metadata // Rssi + DutyCycle
}

// joinPacket implements the core.JoinPacket interface
type joinpacket struct {
	*basehpacket
	metadata Metadata
}

// NewJoinPacket constructs a new JoinPacket
func NewJoinPacket(appEUI lorawan.EUI64, devEUI lorawan.EUI64, devNonce [2]byte, metadata Metadata) JoinPacket {
	return &joinpacket{
		basehpacket: &basehpacket{
			appEUI:  appEUI,
			devEUI:  devEUI,
			payload: devNonce[:],
		},
		metadata: metadata,
	}
}

// DevNonce implements the core.JoinPacket interface
func (p joinpacket) DevNonce() [2]byte {
	return [2]byte{p.basehpacket.payload[0], p.basehpacket.payload[1]}
}

// Metadata implements the core.JoinPacket interface
func (p joinpacket) Metadata() Metadata {
	return p.metadata
}

// String implements the fmt.Stringer interface
func (p joinpacket) String() string {
	return "TODO"
}

type AcceptPacket interface {
	Packet
	AppEUI() lorawan.EUI64
	DevEUI() lorawan.EUI64
	Payload() []byte
	NwkSKey() lorawan.AES128Key
}

// acceptpacket implements the core.AcceptPacket interface
type acceptpacket struct {
	*basehpacket
	nwkSKey lorawan.AES128Key
}

// NewAcceptPacket constructs a new AcceptPacket
func NewAcceptPacket(appEUI lorawan.EUI64, devEUI lorawan.EUI64, payload []byte, nwkSKey lorawan.AES128Key) (AcceptPacket, error) {
	if len(payload) == 0 {
		return nil, errors.New(errors.Structural, "Payload cannot be empty")
	}

	return &acceptpacket{
		basehpacket: &basehpacket{
			appEUI:  appEUI,
			devEUI:  devEUI,
			payload: payload,
		},
		nwkSKey: nwkSKey,
	}, nil
}

// NwkSKey implements the core.AcceptPacket interface
func (p acceptpacket) NwkSKey() lorawan.AES128Key {
	return p.nwkSKey
}

// MarshalBinary implements the encoding.BinaryMarshaler interface
func (p acceptpacket) MarshalBinary() ([]byte, error) {
	data, err := p.basehpacket.MarshalBinary()
	if err != nil {
		return nil, errors.New(errors.Structural, err)
	}
	rw := readwriter.New(data)
	rw.Write(p.nwkSKey)
	return rw.Bytes()
}

// UnmarshalBinary implements the encoding.BinaryUnmarshaler interface
func (p *acceptpacket) UnmarshalBinary(data []byte) error {
	rw := readwriter.New(data)
	rw.Read(func(data []byte) { copy(p.basehpacket.appEUI[:], data) })
	rw.Read(func(data []byte) { copy(p.basehpacket.devEUI[:], data) })
	rw.Read(func(data []byte) { p.basehpacket.payload = data })
	rw.Read(func(data []byte) { copy(p.nwkSKey[:], data) })
	return rw.Err()
}

// String implements the fmt.Stringer interface
func (p acceptpacket) String() string {
	return "TODO"
}

// basehpacket is used to compose other packets
type basehpacket struct {
	appEUI  lorawan.EUI64
	devEUI  lorawan.EUI64
	payload []byte
}

func (p basehpacket) AppEUI() lorawan.EUI64 {
	return p.appEUI
}

func (p basehpacket) DevEUI() lorawan.EUI64 {
	return p.devEUI
}

func (p basehpacket) Payload() []byte {
	return p.payload
}

func (p basehpacket) MarshalBinary() ([]byte, error) {
	rw := readwriter.New(nil)
	rw.Write(p.appEUI)
	rw.Write(p.devEUI)
	rw.Write(p.payload)
	return rw.Bytes()
}

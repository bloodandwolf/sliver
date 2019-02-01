package main

/*

DNS Tunnel Implementation


*/

import (
	"bytes"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	insecureRand "math/rand"
	pb "sliver/protobuf"
	"sliver/server/cryptography"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/miekg/dns"
)

const (
	domainKeyMsg  = "_domainkey"
	blockReqMsg   = "_b"
	clearBlockMsg = "_cb"

	sessionInitMsg    = "_si"
	sessionCleanupMsg = "_sc"
	sessionMsg        = "s"
	sessionHeaderMsg  = "_sh"
	sessionPollingMsg = "_sp"

	sessionIDSize = 8

	// Max TXT record is 255, so (n*8 + 5) / 6 = ~250 (250 bytes per block + 4 byte sequence number)
	byteBlockSize = 185 // Can be as high as n = 187, but we'll leave some slop

	blockIDSize = 6
)

var (
	dnsCharSet = []rune("abcdefghijklmnopqrstuvwxyz0123456789-_")

	sendBlocksMutex = &sync.RWMutex{}
	sendBlocks      = &map[string]*SendBlock{}

	dnsSessionsMutex = &sync.RWMutex{}
	dnsSessions      = &map[string]*DNSSession{}

	blockReassemblerMutex = &sync.RWMutex{}
	blockReassembler      = &map[string][][]byte{}
)

// SendBlock - Data is encoded and split into `Blocks`
type SendBlock struct {
	ID   string
	Data []string
}

// DNSSession - Holds DNS session information
type DNSSession struct {
	ID          string
	SliverName  string
	Sliver      *Sliver
	Key         cryptography.AESKey
	LastCheckin time.Time
}

// --------------------------- DNS SERVER ---------------------------

func startDNSListener(domain string) *dns.Server {

	log.Printf("Starting DNS listener for '%s' ...", domain)

	dns.HandleFunc(".", func(writer dns.ResponseWriter, req *dns.Msg) {
		handleDNSRequest(domain, writer, req)
	})

	server := &dns.Server{Addr: ":53", Net: "udp"}
	return server
}

func handleDNSRequest(domain string, writer dns.ResponseWriter, req *dns.Msg) {

	if len(req.Question) < 1 {
		log.Printf("No questions in DNS request")
		return
	}

	if !dns.IsSubDomain(domain, req.Question[0].Name) {
		log.Printf("Ignoring DNS req, '%s' is not a child of '%s'", req.Question[0].Name, domain)
		return
	}
	subdomain := req.Question[0].Name[:len(req.Question[0].Name)-len(domain)]
	if strings.HasSuffix(subdomain, ".") {
		subdomain = subdomain[:len(subdomain)-1]
	}
	log.Printf("[dns] processing req for subdomain = %s", subdomain)

	resp := &dns.Msg{}
	switch req.Question[0].Qtype {
	case dns.TypeTXT:
		resp = handleTXT(domain, subdomain, req)
	default:
	}

	writer.WriteMsg(resp)
}

func handleTXT(domain string, subdomain string, req *dns.Msg) *dns.Msg {

	q := req.Question[0]
	fields := strings.Split(subdomain, ".")
	resp := new(dns.Msg)
	resp.SetReply(req)
	msgType := fields[len(fields)-1]

	switch msgType {
	case domainKeyMsg: // Send PubKey -  _(nonce).(slivername)._domainkey.example.com
		blockID, size := getDomainKeyFor(domain)
		txt := &dns.TXT{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0},
			Txt: []string{fmt.Sprintf("%s.%d", blockID, size)},
		}
		resp.Answer = append(resp.Answer, txt)
	case blockReqMsg: // Get block: _(nonce).(start).(stop).(block id)._b.example.com
		if len(fields) == 5 {
			startIndex := fields[1]
			stopIndex := fields[2]
			blockID := fields[3]
			txt := &dns.TXT{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0},
				Txt: dnsSendBlocks(blockID, startIndex, stopIndex),
			}
			resp.Answer = append(resp.Answer, txt)
		} else {
			log.Printf("Block request has invalid number of fields %d expected %d", len(fields), 5)
		}
	case sessionInitMsg: // Session init: _(nonce).(session key).(sliver name)._si.example.com
		if len(fields) == 4 {
			encryptedSessionKey := fields[1]
			sliverName := fields[2] // TODO: RSA Encrypt?
			encryptedSessionID, _ := startDNSSession(domain, encryptedSessionKey, sliverName)
			txt := &dns.TXT{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0},
				Txt: []string{encryptedSessionID},
			}
			resp.Answer = append(resp.Answer, txt)
		}
	case sessionHeaderMsg: // Session Header: _(nonce).(pb.DNSBlockHeader).(session id)._sh.example.com
		if len(fields) == 3 {
			encodedDNSBlock := fields[1]
			sessionID := fields[2]
			err := dnsSessionHeader(encodedDNSBlock, sessionID)
			result := 0
			if err != nil {
				result = 1
			}
			txt := &dns.TXT{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0},
				Txt: []string{fmt.Sprintf("%d", result)},
			}
			resp.Answer = append(resp.Answer, txt)
		}
	case sessionMsg: //Session data: _(nonce).(seq|encoded data).(blockHeaderID).(session id).s.example.com
		if len(fields) == 2 {
			data1 := fields[1]
			headerID := fields[2]
			sessionID := fields[3]
			err := dnsSessionMessage([]string{data1}, headerID, sessionID)
			result := 0
			if err != nil {
				result = 1
			}
			txt := &dns.TXT{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0},
				Txt: []string{fmt.Sprintf("%d", result)},
			}
			resp.Answer = append(resp.Answer, txt)
		}
	case clearBlockMsg: // Clear block: _(nonce).(block id)._cb.example.com
		if len(fields) == 3 {
			result := 0
			if clearSendBlock(fields[1]) {
				result = 1
			}
			txt := &dns.TXT{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0},
				Txt: []string{fmt.Sprintf("%d", result)},
			}
			resp.Answer = append(resp.Answer, txt)
		}
	default:
		log.Printf("Unknown msg type '%s' in TXT req", fields[len(fields)-1])
	}

	log.Println("\n" + strings.Repeat("-", 40) + "\n" + resp.String() + "\n" + strings.Repeat("-", 40))

	return resp
}

// --------------------------- DNS SESSION START ---------------------------
func getDomainKeyFor(domain string) (string, int) {
	certPEM, _, _ := GetServerRSACertificatePEM("slivers-rsa", domain)
	blockID, blockSize := storeSendBlocks(certPEM)
	log.Printf("Encoded cert into %d blocks with ID = %s", blockSize, blockID)
	return blockID, blockSize
}

func startDNSSession(domain string, encryptedSessionKey string, sliverName string) (string, error) {
	_, privateKeyPEM, err := GetServerRSACertificatePEM("slivers-rsa", domain)
	if err != nil {
		log.Printf("Failed to fetch RSA key pair %v", err)
		return "", err
	}
	privateKeyBlock, _ := pem.Decode([]byte(privateKeyPEM))
	privateKey, _ := x509.ParsePKCS1PrivateKey(privateKeyBlock.Bytes)
	sessionKey, err := cryptography.RSADecrypt([]byte(encryptedSessionKey), privateKey)
	if err != nil {
		log.Printf("Failed to decrypt RSA message %v", err)
		return "", err
	}
	aesSessionKey, _ := cryptography.AESKeyFromBytes(sessionKey)

	sliver := &Sliver{
		ID:        getHiveID(),
		Send:      make(chan pb.Envelope),
		RespMutex: &sync.RWMutex{},
		Resp:      map[string]chan *pb.Envelope{},
	}

	sessionID := dnsSessionID()
	dnsSessionsMutex.Lock()
	(*dnsSessions)[sessionID] = &DNSSession{
		ID:          sessionID,
		SliverName:  sliverName,
		Sliver:      sliver,
		Key:         aesSessionKey,
		LastCheckin: time.Now(),
	}
	dnsSessionsMutex.Unlock()

	encryptedSessionID, _ := cryptography.GCMEncrypt(aesSessionKey, []byte(sessionID))
	encodedSessionID := base64.RawStdEncoding.EncodeToString(encryptedSessionID)
	return encodedSessionID, nil
}

// --------------------------- DNS SESSION RECV ---------------------------

func dnsSessionHeader(dnsBlockHeaderData string, sessionID string) error {
	dnsSessionsMutex.Lock()
	defer dnsSessionsMutex.Unlock()
	if dnsSession, ok := (*dnsSessions)[sessionID]; ok {
		headerData, err := sessionDecrypt(dnsSession.Key, dnsBlockHeaderData)
		if err != nil {
			log.Printf("Failed to decrypt session message header %v", err)
			return err
		}
		dnsBlockHeader := &pb.DNSBlockHeader{}
		err = proto.Unmarshal(headerData, dnsBlockHeader)
		if err != nil {
			log.Printf("Failed to decode DNSBlockHeader %v", err)
			return err
		}
		blockReassemblerMutex.Lock()
		(*blockReassembler)[dnsBlockHeader.Id] = make([][]byte, dnsBlockHeader.Size)
		blockReassemblerMutex.Unlock()
	}
	return nil
}

// Process an incoming DNS Session message
func dnsSessionMessage(encryptedData []string, encryptedHeaderID string, sessionID string) error {
	dnsSessionsMutex.Lock()
	dnsSession, ok := (*dnsSessions)[sessionID]
	dnsSessionsMutex.Unlock()
	if !ok {
		return errors.New("Invalid sesion ID")
	}
	headerID, err := sessionDecrypt(dnsSession.Key, encryptedHeaderID)
	if err != nil {
		return err
	}

	blockReassemblerMutex.Lock()
	defer blockReassemblerMutex.Unlock() // Lock until we return incase of duplicate messages
	reasm, ok := (*blockReassembler)[string(headerID)]
	if !ok {
		return errors.New("Invalid block header ID")
	}
	for _, ciphertext := range encryptedData {
		rawBuf, err := base32.StdEncoding.DecodeString(ciphertext)
		if err != nil {
			return err
		}
		seqBuf := make([]byte, 4)
		copy(seqBuf, rawBuf[:4])
		seq := int(binary.LittleEndian.Uint32(seqBuf))
		if seq < 0 || len(reasm) <= seq {
			return errors.New("Invalid sequence number")
		}
		reasm[seq] = rawBuf[4:]
	}
	encryptedEnvelopeData := []byte{}
	for index := 0; index < len(reasm); index++ {
		if reasm[index] == nil {
			return nil // Message is incomplete
		}
		encryptedEnvelopeData = append(encryptedEnvelopeData, reasm[index]...)
	}
	envelopeData, err := cryptography.GCMDecrypt(dnsSession.Key, encryptedEnvelopeData)
	if err != nil {
		return err
	}
	envelope := &pb.Envelope{}
	err = proto.Unmarshal(envelopeData, envelope)
	if err != nil {
		log.Printf("Failed to decode Envelope %v", err)
		return err
	}

	if envelope.Id != "" {
		dnsSession.Sliver.RespMutex.Lock()
		if resp, ok := dnsSession.Sliver.Resp[envelope.Id]; ok {
			resp <- envelope
			delete(*blockReassembler, string(headerID)) // We still have the reasm lock
		}
		dnsSession.Sliver.RespMutex.Unlock()
	}

	return nil
}

// --------------------------- DNS SESSION SEND ---------------------------

// Send blocks of data via DNS TXT responses
func dnsSendBlocks(blockID string, startIndex string, stopIndex string) []string {
	start, err := strconv.Atoi(startIndex)
	if err != nil {
		return []string{}
	}
	stop, err := strconv.Atoi(stopIndex)
	if err != nil {
		return []string{}
	}

	if stop < start {
		return []string{}
	}

	log.Printf("Send blocks %d to %d for ID %s", start, stop, blockID)

	sendBlocksMutex.Lock()
	defer sendBlocksMutex.Unlock()
	respBlocks := []string{}
	if block, ok := (*sendBlocks)[blockID]; ok {
		for index := start; index < stop; index++ {
			if index < len(block.Data) {
				respBlocks = append(respBlocks, block.Data[index])
			}
		}
		log.Printf("Sending %d response block(s)", len(respBlocks))
		return respBlocks
	}
	log.Printf("Invalid block ID: %s", blockID)
	return []string{}
}

// Clear send blocks of data from memory
func clearSendBlock(blockID string) bool {
	sendBlocksMutex.Lock()
	defer sendBlocksMutex.Unlock()
	if _, ok := (*sendBlocks)[blockID]; ok {
		delete(*sendBlocks, blockID)
		return true
	}
	return false
}

// Stores encoded blocks fo data into "sendBlocks"
func storeSendBlocks(data []byte) (string, int) {
	blockID := generateBlockID()

	sendBlock := &SendBlock{
		ID:   blockID,
		Data: []string{},
	}
	sequenceNumber := 0
	for index := 0; index < len(data); index += byteBlockSize {
		start := index
		stop := index + byteBlockSize
		if len(data) <= stop {
			stop = len(data) - 1
		}
		seqBuf := new(bytes.Buffer)
		binary.Write(seqBuf, binary.LittleEndian, uint32(sequenceNumber))
		blockBytes := append(seqBuf.Bytes(), data[start:stop]...)
		encoded := "." + base64.RawStdEncoding.EncodeToString(blockBytes)
		log.Printf("Encoded block is %d bytes", len(encoded))
		sendBlock.Data = append(sendBlock.Data, encoded)
		sequenceNumber++
	}
	sendBlocksMutex.Lock()
	(*sendBlocks)[sendBlock.ID] = sendBlock
	sendBlocksMutex.Unlock()
	return sendBlock.ID, len(sendBlock.Data)
}

// --------------------------- HELPERS ---------------------------

// Unique IDs, no need for secure random
func generateBlockID() string {
	insecureRand.Seed(time.Now().UnixNano())
	blockID := []rune{}
	for i := 0; i < blockIDSize; i++ {
		index := insecureRand.Intn(len(dnsCharSet))
		blockID = append(blockID, dnsCharSet[index])
	}
	return string(blockID)
}

// SessionIDs are public parameters in this use case
// so it's only important that they're unique
func dnsSessionID() string {
	insecureRand.Seed(time.Now().UnixNano())
	sessionID := []rune{}
	for i := 0; i < sessionIDSize; i++ {
		index := insecureRand.Intn(len(dnsCharSet))
		sessionID = append(sessionID, dnsCharSet[index])
	}
	return "_" + string(sessionID)
}

// Wrapper around GCMEncrypt & Base32 encode
func sessionDecrypt(sessionKey cryptography.AESKey, data string) ([]byte, error) {
	encryptedData, err := base32.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, err
	}
	return cryptography.GCMDecrypt(sessionKey, encryptedData)
}

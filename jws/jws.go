//go:generate go run internal/cmd/genheader/main.go

// Package jws implements the digital signature on JSON based data
// structures as described in https://tools.ietf.org/html/rfc7515
//
// If you do not care about the details, the only things that you
// would need to use are the following functions:
//
//     jws.Sign(payload, algorithm, key)
//     jws.Verify(encodedjws, algorithm, key)
//
// To sign, simply use `jws.Sign`. `payload` is a []byte buffer that
// contains whatever data you want to sign. `alg` is one of the
// jwa.SignatureAlgorithm constants from package jwa. For RSA and
// ECDSA family of algorithms, you will need to prepare a private key.
// For HMAC family, you just need a []byte value. The `jws.Sign`
// function will return the encoded JWS message on success.
//
// To verify, use `jws.Verify`. It will parse the `encodedjws` buffer
// and verify the result using `algorithm` and `key`. Upon successful
// verification, the original payload is returned, so you can work on it.
package jws

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"unicode"

	"github.com/lestrrat-go/jwx/internal/json"

	"github.com/lestrrat-go/jwx/internal/pool"
	"github.com/lestrrat-go/jwx/jwa"
	"github.com/lestrrat-go/jwx/jwk"
	"github.com/lestrrat-go/jwx/jws/sign"
	"github.com/lestrrat-go/jwx/jws/verify"
	"github.com/pkg/errors"
)

type payloadSigner struct {
	signer    sign.Signer
	key       interface{}
	protected Headers
	public    Headers
}

func (s *payloadSigner) Sign(payload []byte) ([]byte, error) {
	return s.signer.Sign(payload, s.key)
}

func (s *payloadSigner) Algorithm() jwa.SignatureAlgorithm {
	return s.signer.Algorithm()
}

func (s *payloadSigner) ProtectedHeader() Headers {
	return s.protected
}

func (s *payloadSigner) PublicHeader() Headers {
	return s.public
}

// Sign generates a signature for the given payload, and serializes
// it in compact serialization format. In this format you may NOT use
// multiple signers.
//
// It accepts either a raw key (e.g. rsa.PrivateKey, ecdsa.PrivateKey, etc)
// or a jwk.Key, and the name of the algorithm that should be used to sign
// the token.
//
// If the key is a jwk.Key and the key contains a key ID (`kid` field),
// then it is added to the protected header generated by the signature
//
// The algorithm specified in the `alg` parameter must be able to support
// the type of key you provided, otherwise an error is returned.
//
// If you would like to pass custom headers, use the WithHeaders option.
func Sign(payload []byte, alg jwa.SignatureAlgorithm, key interface{}, options ...Option) ([]byte, error) {
	var hdrs Headers
	for _, o := range options {
		switch o.Ident() {
		case identHeaders{}:
			hdrs = o.Value().(Headers)
		}
	}

	if hdrs == nil {
		hdrs = NewHeaders()
	}

	// If the key is a jwk.Key instance, obtain the raw key
	if jwkKey, ok := key.(jwk.Key); ok {
		var tmp interface{}
		if err := jwkKey.Raw(&tmp); err != nil {
			return nil, errors.Wrap(err, `failed to get raw key from jwk.Key instance`)
		}
		key = tmp

		// If we have a key ID specified by this jwk.Key, use that in the header
		if kid := jwkKey.KeyID(); kid != "" {
			if err := hdrs.Set(jwk.KeyIDKey, kid); err != nil {
				return nil, errors.Wrap(err, `set key ID from jwk.Key`)
			}
		}
	}

	signer, err := sign.New(alg)
	if err != nil {
		return nil, errors.Wrap(err, `failed to create signer`)
	}

	if err := hdrs.Set(AlgorithmKey, signer.Algorithm()); err != nil {
		return nil, errors.Wrap(err, `failed to set header`)
	}

	hdrbuf, err := json.Marshal(hdrs)
	if err != nil {
		return nil, errors.Wrap(err, `failed to marshal headers`)
	}

	buf := pool.GetBytesBuffer()
	defer pool.ReleaseBytesBuffer(buf)
	enc := base64.NewEncoder(base64.RawURLEncoding, buf)
	if _, err := enc.Write(hdrbuf); err != nil {
		return nil, errors.Wrap(err, `failed to write headers as base64`)
	}
	if err := enc.Close(); err != nil {
		return nil, errors.Wrap(err, `failed to finalize writing headers as base64`)
	}

	buf.WriteByte('.')
	enc = base64.NewEncoder(base64.RawURLEncoding, buf)
	if _, err := enc.Write(payload); err != nil {
		return nil, errors.Wrap(err, `failed to write payload as base64`)
	}
	if err := enc.Close(); err != nil {
		return nil, errors.Wrap(err, `failed to finalize writing payload as base64`)
	}

	signature, err := signer.Sign(buf.Bytes(), key)
	if err != nil {
		return nil, errors.Wrap(err, `failed to sign payload`)
	}

	buf.WriteByte('.')
	enc = base64.NewEncoder(base64.RawURLEncoding, buf)
	if _, err := enc.Write(signature); err != nil {
		return nil, errors.Wrap(err, `failed to write signature as base64`)
	}
	if err := enc.Close(); err != nil {
		return nil, errors.Wrap(err, `failed to finalize writing signature as base64`)
	}

	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	return result, nil
}

// SignLiteral generates a signature for the given payload and headers, and serializes
// it in compact serialization format. In this format you may NOT use
// multiple signers.
//
func SignLiteral(payload []byte, alg jwa.SignatureAlgorithm, key interface{}, headers []byte) ([]byte, error) {
	signer, err := sign.New(alg)
	if err != nil {
		return nil, errors.Wrap(err, `failed to create signer`)
	}

	buf := pool.GetBytesBuffer()
	defer pool.ReleaseBytesBuffer(buf)

	enc := base64.NewEncoder(base64.RawURLEncoding, buf)
	if _, err := enc.Write(headers); err != nil {
		return nil, errors.Wrap(err, `failed to write headers as base64`)
	}
	if err := enc.Close(); err != nil {
		return nil, errors.Wrap(err, `failed to finalize writing headers as base64`)
	}

	buf.WriteByte('.')
	enc = base64.NewEncoder(base64.RawURLEncoding, buf)
	if _, err := enc.Write(payload); err != nil {
		return nil, errors.Wrap(err, `failed to write payload as base64`)
	}
	if err := enc.Close(); err != nil {
		return nil, errors.Wrap(err, `failed to finalize writing payload as base64`)
	}

	signature, err := signer.Sign(buf.Bytes(), key)
	if err != nil {
		return nil, errors.Wrap(err, `failed to sign payload`)
	}

	buf.WriteByte('.')
	enc = base64.NewEncoder(base64.RawURLEncoding, buf)
	if _, err := enc.Write(signature); err != nil {
		return nil, errors.Wrap(err, `failed to write signature as base64`)
	}
	if err := enc.Close(); err != nil {
		return nil, errors.Wrap(err, `failed to finalize writing signature as base64`)
	}

	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	return result, nil
}

// SignMulti accepts multiple signers via the options parameter,
// and creates a JWS in JSON serialization format that contains
// signatures from applying aforementioned signers.
func SignMulti(payload []byte, options ...Option) ([]byte, error) {
	var signers []PayloadSigner
	for _, o := range options {
		switch o.Ident() {
		case identPayloadSigner{}:
			signers = append(signers, o.Value().(PayloadSigner))
		}
	}

	if len(signers) == 0 {
		return nil, errors.New(`no signers provided`)
	}

	var result Message

	result.payload = payload
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)

	buf := pool.GetBytesBuffer()
	defer pool.ReleaseBytesBuffer(buf)
	for _, signer := range signers {
		protected := signer.ProtectedHeader()
		if protected == nil {
			protected = NewHeaders()
		}

		if err := protected.Set(AlgorithmKey, signer.Algorithm()); err != nil {
			return nil, errors.Wrap(err, `failed to set header`)
		}

		hdrbuf, err := json.Marshal(protected)
		if err != nil {
			return nil, errors.Wrap(err, `failed to marshal headers`)
		}
		encodedHeader := base64.RawURLEncoding.EncodeToString(hdrbuf)

		buf.Reset()
		buf.WriteString(encodedHeader)
		buf.WriteByte('.')
		buf.WriteString(encodedPayload)
		signature, err := signer.Sign(buf.Bytes())
		if err != nil {
			return nil, errors.Wrap(err, `failed to sign payload`)
		}

		result.signatures = append(result.signatures, &Signature{
			headers:   signer.PublicHeader(),
			protected: protected,
			signature: signature,
		})
	}

	return json.Marshal(result)
}

// Verify checks if the given JWS message is verifiable using `alg` and `key`.
// If the verification is successful, `err` is nil, and the content of the
// payload that was signed is returned. If you need more fine-grained
// control of the verification process, manually generate a
// `Verifier` in `verify` subpackage, and call `Verify` method on it.
// If you need to access signatures and JOSE headers in a JWS message,
// use `Parse` function to get `Message` object.
func Verify(buf []byte, alg jwa.SignatureAlgorithm, key interface{}) (ret []byte, err error) {
	verifier, err := verify.New(alg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create verifier")
	}

	buf = bytes.TrimSpace(buf)
	if len(buf) == 0 {
		return nil, errors.New(`attempt to verify empty buffer`)
	}

	if buf[0] == '{' {
		var m Message
		if err := json.Unmarshal(buf, &m); err != nil {
			return nil, errors.Wrap(err, `failed to unmarshal JSON message`)
		}

		enc := base64.RawURLEncoding
		payload := enc.EncodeToString(m.payload)

		buf := pool.GetBytesBuffer()
		defer pool.ReleaseBytesBuffer(buf)
		for i, sig := range m.signatures {
			buf.Reset()
			protected, err := json.Marshal(sig.protected)
			if err != nil {
				return nil, errors.Wrapf(err, `failed to marshal "protected" for signature #%d`, i+1)
			}

			buf.WriteString(enc.EncodeToString(protected))
			buf.WriteByte('.')
			buf.WriteString(payload)

			if err := verifier.Verify(buf.Bytes(), sig.signature, key); err == nil {
				return m.payload, nil
			}
		}
		return nil, errors.New(`could not verify with any of the signatures`)
	}

	protected, payload, signature, err := SplitCompact(bytes.NewReader(buf))
	if err != nil {
		return nil, errors.Wrap(err, `failed extract from compact serialization format`)
	}

	verifyBuf := pool.GetBytesBuffer()
	defer pool.ReleaseBytesBuffer(verifyBuf)

	verifyBuf.Write(protected)
	verifyBuf.WriteByte('.')
	verifyBuf.Write(payload)

	decodedSignature := make([]byte, base64.RawURLEncoding.DecodedLen(len(signature)))
	if _, err := base64.RawURLEncoding.Decode(decodedSignature, signature); err != nil {
		return nil, errors.Wrap(err, `failed to decode signature`)
	}
	if err := verifier.Verify(verifyBuf.Bytes(), decodedSignature, key); err != nil {
		return nil, errors.Wrap(err, `failed to verify message`)
	}

	decodedPayload := make([]byte, base64.RawURLEncoding.DecodedLen(len(payload)))
	if _, err := base64.RawURLEncoding.Decode(decodedPayload, payload); err != nil {
		return nil, errors.Wrap(err, `message verified, failed to decode payload`)
	}
	return decodedPayload, nil
}

// VerifyWithJKU wraps VerifyWithJKUAndContext using the background context.
func VerifyWithJKU(buf []byte, jwkurl string, options ...Option) ([]byte, error) {
	return VerifyWithJKUAndContext(context.Background(), buf, jwkurl, options...)
}

// VerifyWithJKUAndContext verifies the JWS message using a remote JWK
// file represented in the url.
func VerifyWithJKUAndContext(ctx context.Context, buf []byte, jwkurl string, options ...Option) ([]byte, error) {
	key, err := jwk.FetchHTTPWithContext(ctx, jwkurl, options...)
	if err != nil {
		return nil, errors.Wrap(err, `failed to fetch jwk via HTTP`)
	}

	return VerifyWithJWKSet(buf, key, nil)
}

// VerifyWithJWK verifies the JWS message using the specified JWK
func VerifyWithJWK(buf []byte, key jwk.Key) (payload []byte, err error) {
	var rawkey interface{}
	if err := key.Raw(&rawkey); err != nil {
		return nil, errors.Wrap(err, `failed to materialize jwk.Key`)
	}

	payload, err = Verify(buf, jwa.SignatureAlgorithm(key.Algorithm()), rawkey)
	if err != nil {
		return nil, errors.Wrap(err, "failed to verify message")
	}
	return payload, nil
}

// VerifyWithJWKSet verifies the JWS message using JWK key set.
// By default it will only pick up keys that have the "use" key
// set to either "sig" or "enc", but you can override it by
// providing a keyaccept function.
func VerifyWithJWKSet(buf []byte, keyset *jwk.Set, keyaccept JWKAcceptFunc) ([]byte, error) {
	if keyaccept == nil {
		keyaccept = DefaultJWKAcceptor
	}

	for _, key := range keyset.Keys {
		if !keyaccept(key) {
			continue
		}

		payload, err := VerifyWithJWK(buf, key)
		if err == nil {
			return payload, nil
		}
	}

	// refs #140, #141
	//
	// We should not be Wrap()'ing the error here, because of various
	// reasons -- but the fundamental one is that the only value we can get
	// here is the "last error" seen in the above loop, when the symptom
	// that we want to report is that none of the keys worked.
	//
	// Here, we just return that fact, and we do not rely on the value of
	// previous errors.
	return nil, errors.New("failed to verify with any of the keys")
}

// Parse parses contents from the given source and creates a jws.Message
// struct. The input can be in either compact or full JSON serialization.
func Parse(src io.Reader) (m *Message, err error) {
	rdr := bufio.NewReader(src)
	var first rune
	for {
		r, _, err := rdr.ReadRune()
		if err != nil {
			return nil, errors.Wrap(err, `failed to read rune`)
		}
		if !unicode.IsSpace(r) {
			first = r
			if err := rdr.UnreadRune(); err != nil {
				return nil, errors.Wrap(err, `failed to unread rune`)
			}

			break
		}
	}

	var parser func(io.Reader) (*Message, error)
	if first == '{' {
		parser = parseJSON
	} else {
		parser = parseCompact
	}

	m, err = parser(rdr)
	if err != nil {
		return nil, errors.Wrap(err, `failed to parse jws message`)
	}

	return m, nil
}

// ParseString is the same as Parse, but take in a string
func ParseString(s string) (*Message, error) {
	return Parse(strings.NewReader(s))
}

func parseJSON(src io.Reader) (result *Message, err error) {
	var m Message
	if err := json.NewDecoder(src).Decode(&m); err != nil {
		return nil, errors.Wrap(err, `failed to unmarshal jws message`)
	}
	return &m, nil
}

// SplitCompact splits a JWT and returns its three parts
// separately: protected headers, payload and signature.
func SplitCompact(rdr io.Reader) ([]byte, []byte, []byte, error) {
	var protected []byte
	var payload []byte
	var signature []byte
	var periods int = 0
	var state int = 0

	buf := make([]byte, 4096)
	var sofar []byte

	for {
		// read next bytes
		n, err := rdr.Read(buf)
		// return on unexpected read error
		if err != nil && err != io.EOF {
			return nil, nil, nil, errors.Wrap(err, `unexpected end of input`)
		}

		// append to current buffer
		sofar = append(sofar, buf[:n]...)
		// loop to capture multiple '.' in current buffer
		for loop := true; loop; {
			var i = bytes.IndexByte(sofar, '.')
			if i == -1 && err != io.EOF {
				// no '.' found -> exit and read next bytes (outer loop)
				loop = false
				continue
			} else if i == -1 && err == io.EOF {
				// no '.' found -> process rest and exit
				i = len(sofar)
				loop = false
			} else {
				// '.' found
				periods++
			}

			// Reaching this point means we have found a '.' or EOF and process the rest of the buffer
			switch state {
			case 0:
				protected = sofar[:i]
				state++
			case 1:
				payload = sofar[:i]
				state++
			case 2:
				signature = sofar[:i]
			}
			// Shorten current buffer
			if len(sofar) > i {
				sofar = sofar[i+1:]
			}
		}
		// Exit on EOF
		if err == io.EOF {
			break
		}
	}
	if periods != 2 {
		return nil, nil, nil, errors.New(`invalid number of segments`)
	}

	return protected, payload, signature, nil
}

// parseCompact parses a JWS value serialized via compact serialization.
func parseCompact(rdr io.Reader) (m *Message, err error) {
	protected, payload, signature, err := SplitCompact(rdr)
	if err != nil {
		return nil, errors.Wrap(err, `invalid compact serialization format`)
	}

	decodedHeader := make([]byte, base64.RawURLEncoding.DecodedLen(len(protected)))
	if _, err := base64.RawURLEncoding.Decode(decodedHeader, protected); err != nil {
		return nil, errors.Wrap(err, `failed to decode headers`)
	}
	var hdr stdHeaders
	if err := json.Unmarshal(decodedHeader, &hdr); err != nil {
		return nil, errors.Wrap(err, `failed to parse JOSE headers`)
	}

	decodedPayload := make([]byte, base64.RawURLEncoding.DecodedLen(len(payload)))
	if _, err = base64.RawURLEncoding.Decode(decodedPayload, payload); err != nil {
		return nil, errors.Wrap(err, `failed to decode payload`)
	}

	decodedSignature := make([]byte, base64.RawURLEncoding.DecodedLen(len(signature)))
	if _, err := base64.RawURLEncoding.Decode(decodedSignature, signature); err != nil {
		return nil, errors.Wrap(err, `failed to decode signature`)
	}

	var msg Message
	msg.payload = decodedPayload
	msg.signatures = append(msg.signatures, &Signature{
		protected: &hdr,
		signature: decodedSignature,
	})
	return &msg, nil
}

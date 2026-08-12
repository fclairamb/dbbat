package oracle

// ttcClrChunkSize is the payload size of one CLR long-form chunk. 252 is what
// go-ora and the Oracle clients emit, and it is also the largest chunk a
// single-byte length can carry (0xFD/0xFE/0xFF are markers), so the two
// encodings split a value at exactly the same boundaries.
const ttcClrChunkSize = 252

// encodeBigChunkCLRSplit encodes data in the UseBigClrChunks long form —
// 0xFE + (compressed-int chunkLen + chunk)* + 0x00 — splitting it into chunks
// of at most chunkSize bytes, the way a client streams a long value out of a
// fixed buffer. Each chunk length being a TTC compressed integer is the whole
// difference from ttcClr's single-byte lengths; it is the encoder counterpart
// of readCLRVariant(_, true).
func encodeBigChunkCLRSplit(data []byte, chunkSize int) []byte {
	out := make([]byte, 0, len(data)+16)
	out = append(out, 0xFE)

	for i := 0; i < len(data); i += chunkSize {
		end := min(i+chunkSize, len(data))

		out = append(out, ttcCompressedUint(uint64(end-i))...)
		out = append(out, data[i:end]...)
	}

	out = append(out, 0x00)

	return out
}

// ttcClrVariant encodes data as CLR in whichever long-form encoding the session
// negotiated: compressed-int chunk lengths when bigChunks is set (the form an
// upstream that advertised ServerCompileTimeCaps[37]&0x20 parses), single-byte
// chunk lengths otherwise.
//
// Values under the 252-byte short-form limit are byte-identical in both
// encodings and always take the short form, so this is a strict no-op for every
// value dbbat substitutes today (AUTH_SESSKEY 64 hex chars, AUTH_PASSWORD 96,
// AUTH_PBKDF2_SPEEDY_KEY 160). It matters the moment a verifier or key size
// grows past the limit: written with single-byte chunk lengths, a 252-byte
// chunk's 0xFC length reads to a big-chunk parser as "a 0-byte length field"
// and the AUTH message desyncs.
func ttcClrVariant(data []byte, bigChunks bool) []byte {
	if !bigChunks || len(data) < 0xFC {
		return ttcClr(data)
	}

	return encodeBigChunkCLRSplit(data, ttcClrChunkSize)
}

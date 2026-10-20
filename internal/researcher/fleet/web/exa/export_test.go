package exa

// DecodeSearchReply and DecodeContentsReply expose the client's reply
// decoding to exa_test, which also drives exatest's fake and so cannot see
// the unexported envelopes directly.
func DecodeSearchReply(data []byte) (searchEnvelope, error) {
	var env searchEnvelope
	err := decodeObject(data, &env)
	return env, err
}

func DecodeContentsReply(data []byte) (contentsEnvelope, error) {
	var env contentsEnvelope
	err := decodeObject(data, &env)
	return env, err
}

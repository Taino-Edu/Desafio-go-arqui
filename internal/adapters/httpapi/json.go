package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxBodyBytes = 64 << 10 // 64 KiB: suficiente para qualquer operação

var errBadRequest = errors.New("bad request")

// decodeJSON lê exatamente um objeto JSON, recusando campos desconhecidos,
// corpo vazio, lixo depois do objeto e corpos grandes demais.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %s", errBadRequest, describeJSONError(err))
	}
	if dec.More() {
		return fmt.Errorf("%w: unexpected data after JSON object", errBadRequest)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// describeJSONError produz mensagens para o cliente sem expor nomes de
// tipos internos do Go.
func describeJSONError(err error) string {
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	var maxErr *http.MaxBytesError
	switch {
	case errors.Is(err, io.EOF):
		return "empty body"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &syntaxErr):
		return "malformed JSON"
	case errors.As(err, &typeErr):
		if typeErr.Field == "" {
			return "body must be a JSON object"
		}
		return fmt.Sprintf("field %q must be a JSON %s", typeErr.Field, jsonKind(typeErr.Type.Kind().String()))
	case errors.As(err, &maxErr):
		return "body too large"
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return "unknown field " + strings.TrimPrefix(err.Error(), "json: unknown field ")
	default:
		return "invalid JSON body"
	}
}

func jsonKind(goKind string) string {
	switch goKind {
	case "string":
		return "string"
	case "struct", "map", "ptr":
		return "object"
	case "slice", "array":
		return "array"
	case "bool":
		return "boolean"
	default:
		return "number"
	}
}

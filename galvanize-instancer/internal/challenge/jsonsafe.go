package challenge

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// jsonSafe returns v (a parsed YAML document) with only values JSON can
// encode: YAML allows mappings with non-string keys (1: x, true: y, nested
// mappings as keys) and .nan / .inf numbers, which would make encoding the
// whole challenge catalog fail. Scalar keys become strings when that loses
// nothing; anything else is an error naming where it is.
func jsonSafe(v interface{}, at string) (interface{}, error) {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			n, err := jsonSafe(val, at+"."+k)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			var key string
			switch kk := k.(type) {
			case string:
				key = kk
			case int, int64, uint64, bool:
				key = fmt.Sprint(kk)
			case float64:
				if math.IsNaN(kk) || math.IsInf(kk, 0) {
					return nil, fmt.Errorf("%s: unsupported key %v", where(at), kk)
				}
				key = fmt.Sprint(kk)
			case nil:
				key = "null"
			default:
				return nil, fmt.Errorf("%s: unsupported key of type %T", where(at), k)
			}
			if _, dup := out[key]; dup {
				return nil, fmt.Errorf("%s: key %q appears twice once keys are strings", where(at), key)
			}
			n, err := jsonSafe(val, at+"."+key)
			if err != nil {
				return nil, err
			}
			out[key] = n
		}
		return out, nil
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			n, err := jsonSafe(val, fmt.Sprintf("%s[%d]", at, i))
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return nil, fmt.Errorf("%s: %v is not a JSON number", where(at), t)
		}
		return t, nil
	case nil, string, bool, int, int64, uint64, time.Time:
		return t, nil
	case []byte:
		return string(t), nil
	default:
		return nil, fmt.Errorf("%s: unsupported value of type %T", where(at), v)
	}
}

func where(at string) string {
	if at == "" {
		return "the document"
	}
	return strings.TrimPrefix(at, ".")
}

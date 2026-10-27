package agentid_test

import (
	"reflect"
	"slices"
	"unicode"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/agentid"
)

type stringMethod struct{ name, result string }

type providerSurface struct {
	stringMethods  []stringMethod
	sourceExported bool
}

func (s providerSurface) names() []string {
	out := make([]string, 0, len(s.stringMethods))
	for _, m := range s.stringMethods {
		out = append(out, m.name)
	}
	slices.Sort(out)
	return out
}

// reflectProvider calls every exported method of p that takes nothing and
// answers a string, and inspects the assertion source's dynamic type.
func reflectProvider(p *agentid.Provider) providerSurface {
	var out providerSurface
	value := reflect.ValueOf(p)
	stringType := reflect.TypeFor[string]()
	for i := range value.NumMethod() {
		method := value.Type().Method(i)
		if method.Type.NumIn() != 1 || method.Type.NumOut() != 1 || method.Type.Out(0) != stringType {
			continue
		}
		result := value.Method(i).Call(nil)[0].String()
		out.stringMethods = append(out.stringMethods, stringMethod{name: method.Name, result: result})
	}
	source := reflect.TypeOf(p.AssertionSource())
	for source.Kind() == reflect.Pointer {
		source = source.Elem()
	}
	out.sourceExported = unicode.IsUpper([]rune(source.Name())[0])
	for i := range source.NumField() {
		if source.Field(i).IsExported() {
			out.sourceExported = true
		}
	}
	return out
}

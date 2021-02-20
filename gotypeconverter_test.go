package gotypeconverter_test

import (
	"flag"
	"io/ioutil"
	"os"
	"testing"

	"github.com/fuji8/gotypeconverter"
	"github.com/gostaticanalysis/codegen/codegentest"
)

var flagUpdate bool

type flagValue struct {
	s string
	d string
	o string
}

func TestMain(m *testing.M) {
	flag.BoolVar(&flagUpdate, "update", false, "update the golden files")
	flag.Parse()
	os.Exit(m.Run())
}

func TestGenerator(t *testing.T) {
	m := map[string]flagValue{
		"external": flagValue{
			s: "[]echo.Echo",
			d: "externalDst",
			o: "",
		},
		"pointer": flagValue{
			s: "[]*pointerSrc",
			d: "[]*pointerDst",
			o: "",
		},
		"samename": flagValue{
			s: "Hoge",
			d: "foo.Hoge",
			o: "",
		},
	}

	fileInfos, err := ioutil.ReadDir(codegentest.TestData() + "/src")
	if err != nil {
		panic(err)
	}
	for _, fi := range fileInfos {
		if fi.IsDir() {
			fv, ok := m[fi.Name()]
			if !ok {
				gotypeconverter.Generator.Flags.Set("s", fi.Name()+"Src")
				gotypeconverter.Generator.Flags.Set("d", fi.Name()+"Dst")
				gotypeconverter.Generator.Flags.Set("o", "")
			} else {
				gotypeconverter.Generator.Flags.Set("s", fv.s)
				gotypeconverter.Generator.Flags.Set("d", fv.d)
				gotypeconverter.Generator.Flags.Set("o", fv.o)
			}

			gotypeconverter.CreateTmpFile(codegentest.TestData() + "/src/" + fi.Name())

			rs := codegentest.Run(t, codegentest.TestData(), gotypeconverter.Generator, fi.Name())
			codegentest.Golden(t, rs, flagUpdate)
		}
	}
}

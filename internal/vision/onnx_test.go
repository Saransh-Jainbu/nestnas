package vision

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// NN_ONNX=<model> NN_ONNX_IN=<raw float32 file> NN_ONNX_SHAPE=1,3,112,112 NN_ONNX_OUT=<folder>
// writes every output so a script can compare it with onnxruntime.
func TestONNXDump(t *testing.T) {
	model, in, shape, out := os.Getenv("NN_ONNX"), os.Getenv("NN_ONNX_IN"), os.Getenv("NN_ONNX_SHAPE"), os.Getenv("NN_ONNX_OUT")
	if model == "" || in == "" {
		t.Skip("no model given")
	}
	net, err := LoadNet(model)
	if err != nil {
		t.Fatal(err)
	}
	var sh []int
	for _, p := range strings.Split(shape, ",") {
		n, _ := strconv.Atoi(p)
		sh = append(sh, n)
	}
	raw, _ := os.ReadFile(in)
	x := &tensor{sh, make([]float32, len(raw)/4)}
	for i := range x.data {
		x.data[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	res, err := net.Run(x)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range res {
		b := make([]byte, len(v.data)*4)
		for i, f := range v.data {
			binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
		}
		os.WriteFile(filepath.Join(out, name+".f32"), b, 0o644)
	}
}

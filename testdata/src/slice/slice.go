package slice

type sliceSrc struct {
	ns  []float64
	n   float64
	arr []struct {
		foo string
		bar int
	}
	strs []string
	sarr []struct {
		foo string
		bar int
	}
}

type sliceDst struct {
	ns  float64
	n   []float64
	arr struct {
		bar int
	}
	strs []string
	sarr []struct {
		bar int
		hii float32
	}
}

func main() {
}

package pkg

// Greeter builds greeting strings.
type Greeter struct{}

// Greet returns a greeting.
func (g Greeter) Greet(name string) string {
	return "hello " + name
}

// NewGreeter constructs a Greeter.
func NewGreeter() *Greeter {
	return &Greeter{}
}

type helper struct{}

func (h *helper) secret() {}

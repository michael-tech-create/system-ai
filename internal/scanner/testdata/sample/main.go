package main

import (
	"fmt"

	"example.com/sample/pkg"
)

func main() {
	fmt.Println(pkg.Greeter{}.Greet("world"))
}

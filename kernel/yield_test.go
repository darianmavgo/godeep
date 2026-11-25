package kernel

import "time"

func runtimeYield() { time.Sleep(5 * time.Millisecond) }

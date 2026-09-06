package main

import "time"

// nowLocal is the local wall clock, indirected so tests can pin it.
var nowLocal = time.Now

// Command service is the eK8s hashing service entrypoint: it wires the
// fixed hashcore and evolvable wrapper into HTTP routes, healthz/readyz,
// and graceful shutdown. Never touched by mutation (DESIGN.md sec 5).
package main

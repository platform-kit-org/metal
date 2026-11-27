// Package lint holds typed checks of rules that the compiler does not
// enforce. Each check is a test: it loads the module with go/types, runs on
// a fixture whose marked lines must be exactly what it finds, and then on
// the module, where it must find nothing.
//
// The develop page of the documentation site lists the rules, and the mise
// task that runs each check.
package lint

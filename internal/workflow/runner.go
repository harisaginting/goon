package workflow

// The StageRunner and its linear declarative pipeline were replaced by the
// role-graph executor in graph.go (runGraph + per-role node runners). This
// file is intentionally empty apart from the package clause; the shared
// helpers it used to hold (StageState, validateStages, renderTemplate,
// templateFuncs, targetsOf, isFalsy, on-error policies) now live in graph.go.

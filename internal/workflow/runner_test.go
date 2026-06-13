package workflow

// The StageRunner-based declarative tests were replaced by the role-graph
// tests in graph_test.go (which drive Engine.runGraph). The shared helpers
// (ticketFixture, fakeTool, seqTool, notifyStage) and the ported coverage
// (routing, loop, reject, template funcs, validation) now live there.

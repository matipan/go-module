module example.com/go-module-with-replace

go 1.25

require example.com/replaced-lib v0.0.0

replace example.com/replaced-lib => ../replaced-lib

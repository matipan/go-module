module example.com/included-gomod-replace-root

go 1.25

require example.com/distconsts v0.0.0

replace example.com/distconsts => ./engine/distconsts

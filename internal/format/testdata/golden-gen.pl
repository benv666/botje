#!/usr/bin/perl
# Golden parity generator for go-botje internal/format.
# Loads the REAL Perl botje code (Log.pm, Functions.pm, and extracted
# subs from IRC.pm) and dumps input/output pairs as JSON.
use strict;
use warnings;
use utf8;
use JSON::PP;
use POSIX ();
use Data::Dumper;

package main;
sub mdebug {}
sub debug {}

my $src = "/home/benv/code/go-botje/reference/src/botje-hg";
require "$src/modules/core/Log.pm";
require "$src/modules/core/Functions.pm";

# Extract %colorCodes + translateColors and wrapText/splitMessageData
# verbatim from IRC.pm and eval them into package Extracted.
sub extract {
    my ($from, $to) = @_;
    open my $fh, '<:utf8', "$src/modules/core/IRC.pm" or die $!;
    my @lines = <$fh>;
    close $fh;
    return join('', @lines[$from-1 .. $to-1]);
}
my $code = "package Extracted;\nuse strict; use warnings;\nsub debug {}\n"
    . extract(2447, 2506)    # %colorCodes + sub translateColors
    . extract(1739, 1758)    # sub splitMessageData
    . extract(1768, 1815)    # sub wrapText
    . "1;\n";
eval $code or die "eval extracted: $@";

my %out;

# --- colorize pipeline: {x} tags -> ANSI (subColorTags) -> mIRC (translateColors)
my @colorizeCases = (
    'plain text, no tags',
    '{/}reset', '{r}brown', '{R}red', '{g}green', '{G}bgreen',
    '{y}orange', '{Y}yellow', '{b}blue', '{m}purple', '{M}bpurple',
    '{c}cyan', '{C}bcyan', '{w}grey', '{W}bwhite', '{B}boldflag', '{_}under',
    '{B}{r}boldstate-red',
    '{R}bright{g}stays-bright-green',
    '{R}bright{/}{g}reset-then-green',
    '{r}a{y}b{g}c{/}done',
    '{q}unknown tag stripped',
    '{r},5 comma fixup',
    'mid{c}word{/}tags',
    "multi {g}line\nsecond {r}line",
    '{_}underline{/}off',
    'trailing tag{y}',
    '{}',
    'unclosed {r brace',
);
$out{colorize} = [ map {
    { in => $_, ansi => Log::subColorTags($_),
      mirc => Extracted::translateColors(Log::subColorTags($_)) }
} @colorizeCases ];

# --- wrapText
my @wrapCases = (
    ['short line', 448],
    ['a b c d e f g h i j', 8],
    ['aaaaaaaaaaaaaaaaaaaaaaaaa', 10],                    # one giant word
    ['word ' . ('x' x 30) . ' tail', 12],                 # giant word mid-sentence
    ['een twee drie vier vijf zes zeven acht', 12],
    ["h\x{e9}h\x{e9} b\x{e9}b\x{e9} caf\x{e9} r\x{f6}sti \x{fc}ber", 10],  # 2-byte chars
    ["\x{1f37a}\x{1f37a}\x{1f37a}\x{1f37a}\x{1f37a}", 8], # 4-byte chars, giant word
    # NOT here: "mixed 🍺 beer ..." style cases where a multibyte word gets
    # joined with a following word: perl's 'use bytes' concat corrupts the
    # bytes (live bug, fixed in Go, hand-written expectation in Go tests)
    ['exactfit12 exactfit12', 23],
    ['twelve bytes', 12],
    ["tabs\tand  multiple   spaces", 10],
);
# the live bot decodes IRC input with Encode FB_QUIET, so strings are
# always UTF8-upgraded internally; force the same here or 'use bytes'
# in wrapText counts internal native bytes instead of UTF-8 bytes
$out{wraptext} = [ map {
    my ($in, $max) = @$_;
    utf8::upgrade($in);
    { in => $in, max => $max, out => Extracted::wrapText($in, $max) }
} @wrapCases ];

# --- splitMessageData
my @splitCases = (
    ['PRIVMSG #testing :', 'short message'],
    ['PRIVMSG #testing :', 'x' x 500],
    ['PRIVMSG #t :', ('word ' x 120) . 'end'],
);
$out{splitmessagedata} = [ map {
    my ($prefix, $msg) = @$_;
    utf8::upgrade($msg);
    { prefix => $prefix, msg => $msg,
      out => Extracted::splitMessageData($prefix, $msg) }
} @splitCases ];

# --- sparkline
my @sparkCases = (
    [[1,5,3], 1],
    [[1,2,3,4,5,6,7,8,9], 1],
    [[9,8,7,6,5,4,3,2,1], 1],
    [[5,5,5], 1],
    [[3,1,4,1,5,9,2,6], 2],
    [[], 1],
    [[42], 1],
    [[-3,0,3], 1],
    [[100,101,100,99,100], 3],
    [[0.1,0.2,0.15,0.3], 1],
    [[7,7,7,8,7,7], 1],
    [[1,5,3], 0],
    [[0,4.5,8], 1],     # v exactly on the int(x+0.5) rounding boundary
    [[0,3.7,8], 2],
);
$out{sparkline} = [ map {
    my $r = Functions::sparkline($_->[0], $_->[1]);
    { in => $_->[0], rows => $_->[1], out => (ref $r ? $r : [$r]) }
} @sparkCases ];

# --- tfcprint
my @tfcCases = (
    # [format, color, args, mode]
    ['10|20', 1, undef, undef],
    ['10%s|20%s', undef, undef, 1],
    ['10%s|20%s', 'G|/', ['hello', 'world'], undef],
    ['8%s|8%s', 'r|g', ['left', 'right'], undef],
    ['8%s', 'y', ['toolongvaluehere'], undef],            # multi-line continuation
    ['12%s|8%s', 'C|w', ['some{g}col{/}text', 'ok'], undef], # colored arg + reset-to-format-color
    ['6%s|30%s', 'W|/', ['id', 'x' x 40], undef],
    ['3|3', 1, undef, undef],                              # too short -> error string
    ['10%s|20%s', 'G', ['a', 'b'], undef],                 # colors != args -> error string
);
$out{tfcprint} = [ map {
    { format => $_->[0], color => $_->[1], args => $_->[2], mode => $_->[3],
      out => Functions::tfcprint(@$_) }
} @tfcCases ];

binmode STDOUT, ':utf8';
print JSON::PP->new->canonical->pretty->encode(\%out);

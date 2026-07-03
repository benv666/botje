#!/usr/bin/perl
# Storable .dat -> JSON dumper for the go-botje migration.
# Usage: dump.pl <module.dat>   (JSON on stdout)
#
# Runs with a plain system perl: Storable and JSON::PP are core.
# Blessed refs (XML::LibXML leftovers, DateTime in Lastseen) are
# unblessed to plain data so JSON can carry them.
use strict;
use warnings;
use Storable ();
use JSON::PP ();
use Scalar::Util qw(blessed reftype);

my ($file) = @ARGV;
die "usage: dump.pl <module.dat>\n" unless defined $file && -r $file;

my $data = Storable::retrieve($file);
strip($data);

binmode STDOUT, ':utf8';
print JSON::PP->new->canonical->allow_nonref->convert_blessed->encode($data);

# strip recursively replaces blessed refs with their underlying data
# (DateTime objects become their epoch when they can say it).
sub strip {
    my ($node) = @_;
    my $type = reftype($node) // '';
    if ($type eq 'HASH') {
        for my $k (keys %$node) {
            my $v = $node->{$k};
            if (blessed($v)) {
                $node->{$k} = unbless($v);
                $v = $node->{$k};
            }
            strip($v) if ref $v;
        }
    }
    elsif ($type eq 'ARRAY') {
        for my $i (0 .. $#$node) {
            if (blessed($node->[$i])) {
                $node->[$i] = unbless($node->[$i]);
            }
            strip($node->[$i]) if ref $node->[$i];
        }
    }
}

sub unbless {
    my ($obj) = @_;
    return $obj->epoch if $obj->can('epoch'); # DateTime and friends
    my $type = reftype($obj) // '';
    return {%$obj} if $type eq 'HASH';
    return [@$obj] if $type eq 'ARRAY';
    return "$obj";
}

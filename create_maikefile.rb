#!/usr/local/opt/ruby/bin/ruby

unless ENV.has_key?("HOME")
  warn("HOME env variable not set")
  exit(1)
end

dl_file = File.join(ENV["HOME"], "Downloads", "verifien.zip")
unless File.file?(dl_file)
  warn("No zip file #{dl_file}")
  exit(1)
end

doc_dir = File.join(ENV["HOME"], "Documents")
unless File.directory?(doc_dir)
  warn("No directory: #{doc_dir}")
  exit(1)
end

doc_file = File.join(doc_dir, "verifien.zip")
vdir = File.join(doc_dir, "verifien")
begin
  File.unlink(doc_file)
rescue 
end
File.rename(dl_file, doc_file)
Dir.chdir(doc_dir)
system("unzip -u verifien.zip")
begin
  File.unlink(doc_file)
rescue
end
Dir.chdir(vdir)
begin
  File.unlink("Makefile")
rescue
end
gfiles = Dir.glob("*.go").concat(Dir.glob("*/*.go")).delete_if{|name| name.end_with?("_test.go")}

File.open("Makefile", "w"){|fp|
  fp.puts "FILES = #{gfiles.join(' ')}"
  fp.puts "# .DELETE_ON_ERROR: verifien"
  fp.puts "verifien : go.mod go.sum ${FILES} vendor/modules.txt"
  fp.puts "\tgo build -o verifien -mod vendor"
  fp.puts "\tchmod 755 verifien"
  fp.puts ""
  fp.puts "go.mod :"
  fp.puts "\trm -fr go.mod go.sum vendor"
  fp.puts "\tgo mod init verifien"
  fp.puts ""
  fp.puts "go.sum : go.mod"
  fp.puts "\trm -fr go.sum vendor"
  fp.puts "\tgo mod tidy"
  fp.puts ""
  fp.puts "vendor vendor/modules.txt : go.sum go.mod"
  fp.puts "\trm -fr vendor"
  fp.puts "\tgo mod vendor"
  fp.puts ""
  fp.puts ".PHONY: clean"
  fp.puts "clean:"
  fp.puts "\trm -fr cover.out verifien vendor go.sum go.mod"
  fp.puts ""
}

#system("make clean")
#system("make")


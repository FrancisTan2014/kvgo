(defproject jepsen.kvgo "0.1.0-SNAPSHOT"
  :description "Jepsen tests for kv-go distributed key-value store"
  :license {:name "Eclipse Public License"
            :url  "http://www.eclipse.org/legal/epl-v10.html"}
  :main jepsen.kvgo
  :jvm-opts ["-Xmx8g" "-Xms2g" "-server"]
  :target-path "bin/target"
  :dependencies [[org.clojure/clojure "1.11.1"]
                 [jepsen "0.3.6"]
                 [clj-http "3.12.3"]]
  :repl-options {:init-ns jepsen.kvgo})

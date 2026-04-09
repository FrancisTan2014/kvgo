(ns jepsen.kvgo
  "Jepsen test suite for kv-go: a Raft-based distributed key-value store.

  This namespace owns shared infrastructure — db lifecycle, test construction,
  CLI entry point. Clients and HTTP helpers live in jepsen.kvgo.support.
  Individual workloads live under jepsen.kvgo.{basic,partition,...}."
  (:require [clojure.string :as str]
            [clojure.tools.logging :refer [info warn]]
            [jepsen [cli :as cli]
                    [control :as c]
                    [db :as db]
                    [nemesis :as nemesis]
                    [os :as os]
                    [store :as store]
                    [tests :as tests]]
            [jepsen.control.util :as cu]
            [jepsen.kvgo.basic :as basic]
            [jepsen.kvgo.partition :as partition]))

;; ---------------------------------------------------------------------------
;; Configuration
;; ---------------------------------------------------------------------------

(def dir       "/opt/kvgo")
(def binary    (str dir "/kv-server"))
(def data-dir  "/var/lib/kvgo")
(def kv-port   4000)
(def raft-port 5000)
(def http-port 8080)
(def log-file  (str data-dir "/kvgo.log"))
(def pid-file  (str data-dir "/kvgo.pid"))

;; ---------------------------------------------------------------------------
;; Node helpers
;; ---------------------------------------------------------------------------

(defn node-id
  "Extracts a numeric ID from a Jepsen node name like \"n1\" -> 1."
  [node]
  (Long/parseLong (subs (name node) 1)))

(defn peers-str
  "Builds the -peers flag value for a given node, excluding itself.
  Example: \"2=n2:5000,3=n3:5000\""
  [test node]
  (->> (:nodes test)
       (remove #{node})
       (map (fn [n] (str (node-id n) "=" (name n) ":" raft-port)))
       (str/join ",")))

;; ---------------------------------------------------------------------------
;; Database automation
;; ---------------------------------------------------------------------------

(defn db
  "Jepsen database lifecycle for kv-go.
  Assumes the kv-server binary is already deployed to /opt/kvgo/kv-server
  on each node."
  []
  (reify
    db/DB
    (setup! [_ test node]
      (info "Setting up kv-go on" node)
      (c/exec :mkdir :-p data-dir)
      (cu/start-daemon!
        {:logfile log-file
         :pidfile pid-file
         :chdir   dir}
        binary
        :-node-id  (node-id node)
        :-data-dir data-dir
        :-host     "0.0.0.0"
        :-port     kv-port
        :-raft-port raft-port
        :-http-port http-port
        :-peers     (peers-str test node))
      ;; Give the cluster time to elect a leader.
      (Thread/sleep 5000))

    (teardown! [_ test node]
      (info "Tearing down kv-go on" node)
      (cu/stop-daemon! binary pid-file)
      (c/exec :rm :-rf data-dir))

    db/LogFiles
    (log-files [_ test node]
      [log-file])))

;; ---------------------------------------------------------------------------
;; Workload registry
;; ---------------------------------------------------------------------------

(def workloads
  "Map of workload name → workload constructor."
  {"basic"     basic/workload
   "partition" partition/workload})

;; ---------------------------------------------------------------------------
;; Test definition
;; ---------------------------------------------------------------------------

(defn kvgo-test
  "Constructs a Jepsen test map from CLI options.
  Workloads return {:client :generator :checker :nemesis}."
  [opts]
  (let [workload-name (get opts :workload "basic")
        workload-fn   (get workloads workload-name)]
    (when (nil? workload-fn)
      (throw (IllegalArgumentException.
               (str "Unknown workload: " workload-name
                    ". Available: " (str/join ", " (keys workloads))))))
    (let [wl (workload-fn opts)]
      (merge tests/noop-test
             opts
             {:pure-generators true
              :name            (str "kvgo-" workload-name)
              :os              os/noop
              :db              (db)
              :client          (:client wl)
              :nemesis         (:nemesis wl nemesis/noop)
              :checker         (:checker wl)
              :generator       (:generator wl)}))))

;; ---------------------------------------------------------------------------
;; CLI entry point
;; ---------------------------------------------------------------------------

(def cli-opts
  "Additional CLI options for kvgo tests."
  [["-w" "--workload NAME" "Workload to run (basic, ...)"
    :default "basic"]])

(defn -main
  "Jepsen CLI entry point."
  [& args]
  (alter-var-root #'store/base-dir (constantly "store"))
  (cli/run! (merge (cli/single-test-cmd {:test-fn  kvgo-test
                                         :opt-spec cli-opts})
                   (cli/serve-cmd))
            args))

(ns jepsen.kvgo.basic
  "Basic workload: concurrent reads and writes against a single register.
  No nemesis — proves the plumbing works and baseline correctness holds."
  (:require [jepsen [checker :as checker]
                    [generator :as gen]
                    [nemesis :as nemesis]]
            [jepsen.checker.timeline :as timeline]
            [knossos.model :as model]))

(defn r [_ _] {:type :invoke, :f :read, :value nil})
(defn w [_ _] {:type :invoke, :f :write, :value (rand-int 5)})

(defn workload
  "Returns a map of :generator, :checker, :nemesis for the basic register test."
  [opts]
  {:nemesis   nemesis/noop
   :checker   (checker/compose
                {:linear   (checker/linearizable
                             {:model     (model/register nil)
                              :algorithm :wgl})
                 :perf     (checker/perf)
                 :timeline (timeline/html)})
   :generator (->> (gen/mix [r w])
                   (gen/stagger 1/5)
                   (gen/nemesis nil)
                   (gen/time-limit (:time-limit opts 30)))})

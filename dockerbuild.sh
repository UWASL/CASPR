kubectl delete -f my-scheduler.yaml

make 

sudo docker build -t saraqnb/$1:1.0 . 
sudo docker push saraqnb/$1:1.0 

#sed '73d' my-scheduler.yaml > temp.yaml 
 
#sed "73 i \ \ \ \ \ \ \ \ image:saraqnb/$1:1.0" temp.yaml > my-scheduler.yaml 

#kubectl create -f my-scheduler.yaml

rm /storage/spark/driver.yaml 
rm /storage/spark/worker.yaml 

echo "apiVersion: v1
kind: Pod
metadata:
  name: driver
spec:
  schedulerName: my-scheduler
  containers:
  - name: my-scheduler
    image: saraqnb/$1:1.0 " >> /storage/spark/driver.yaml 

echo "apiVersion: v1
kind: Pod
metadata:
  name: worker
spec:
  schedulerName: my-scheduler
  containers:
  - name: my-scheduler
    image: saraqnb/$1:1.0 " >> /storage/spark/worker.yaml 

